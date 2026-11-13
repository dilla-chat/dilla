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
    Window, canvas, div, img, px, relative,
};

use dilla_desktop_ui::components::icon_button::{IconButton, IconButtonSize};
use dilla_desktop_ui::components::{Avatar, AvatarShape, AvatarSize};
use dilla_desktop_ui::text::line;
use dilla_desktop_ui::tokens::{color, font, layout, radius, text};
use dilla_desktop_ui::{Icon, IconName};

use crate::strings::{t, tf};
use crate::{tracked, tracked_box};

use session::MediaSession;

/// What the voice hooks draw from: the newest state, the running client and its call's media, and the stage's own
/// view state (which tile is focused, which is selected).
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
    /// The AFK page's Come back: the shell's way to a text channel this person can send in ([`set_come_back`]).
    come_back: Option<crate::screens::ring::OnComeBack>,
}

impl Global for VoiceUi {}

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

/// Keeps the voice hooks current: the shell calls it with every new state (and the client once it runs). Starts the
/// voice area's following of the instance and the call's media session once per client.
pub fn sync(state: Arc<AppState>, client: Option<&Rc<Client>>, cx: &mut App) {
    if !cx.has_global::<VoiceUi>() {
        cx.set_global(VoiceUi::default());
    }
    let fresh_client = {
        let ui = cx.global::<VoiceUi>();
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
        let ui = cx.global_mut::<VoiceUi>();
        ui.client = Some(client.clone());
        ui.session = session;
        ui.told_in_call = None;
        ui.told_roster = None;
        repaint_on_video(cx);
    }
    if let Some(client) = client {
        tell_ring(&state, client, cx);
    }
    let ui = cx.global_mut::<VoiceUi>();
    if ui.state.voice.call.phase != CallPhase::Idle && state.voice.call.phase == CallPhase::Idle {
        ui.focused = None;
        ui.selected = 0;
    }
    ui.state = state;
}

/// Repaints while new remote video frames arrive (at most 30 times a second).
fn repaint_on_video(cx: &mut App) {
    cx.spawn(async move |cx| {
        loop {
            cx.background_executor()
                .timer(Duration::from_millis(33))
                .await;
            let moved = cx.update(|cx| {
                let ui = cx.try_global::<VoiceUi>()?;
                let seq = ui.session.as_ref()?.video_seq.load(Ordering::Relaxed);
                (seq != ui.video_seq).then_some(seq)
            });
            if let Some(seq) = moved {
                cx.update(|cx| {
                    cx.global_mut::<VoiceUi>().video_seq = seq;
                    cx.refresh_windows();
                });
            }
        }
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
fn tell_ring(state: &AppState, client: &Rc<Client>, cx: &mut App) {
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
    let ui = cx.global_mut::<VoiceUi>();
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

/// The shell's way out of the AFK page (Shell.tsx `onLeaveAfk`), set on each render of the shell.
pub fn set_come_back(come_back: crate::screens::ring::OnComeBack, cx: &mut App) {
    if !cx.has_global::<VoiceUi>() {
        cx.set_global(VoiceUi::default());
    }
    cx.global_mut::<VoiceUi>().come_back = Some(come_back);
}

/// The slot context of the stage's channel, from what the voice hooks hold (the ring area's stage reads take one).
fn stage_ctx(ui: &VoiceUi, channel: &Channel) -> crate::slots::SlotCx {
    crate::slots::SlotCx {
        state: ui.state.clone(),
        client: ui.client.clone(),
        community: Some(channel.community_id),
        channel: Some(channel.id),
        clock: crate::screens::shell::Clock::Local,
    }
}

/// The AFK channel's page (VoiceView.tsx `AfkPage`), in place of the stage, when `channel` is its server's AFK channel:
/// the stage's title bar, the stage notice and the ring area's page (Task 18: who is away, the server's timeout, Go AFK
/// or Come back). A server whose AFK snapshot is not read yet is asked for it (`Ring::load_afk` reads one at a time) and
/// shows the ordinary stage meanwhile.
fn afk_stage(channel: &Channel, cx: &mut App) -> Option<AnyElement> {
    let ui = ui(cx)?;
    let community = channel.community_id;
    let Some(snapshot) = ui.state.ring.afk.get(&community) else {
        if let Some(client) = &ui.client {
            drop(client.ring().load_afk(community));
        }
        return None;
    };
    if snapshot.channel != Some(channel.id) {
        return None;
    }
    let ctx = stage_ctx(ui, channel);
    let come_back = ui
        .come_back
        .clone()
        .unwrap_or_else(|| Rc::new(|_: &mut Window, _: &mut App| {}));
    let page = crate::screens::ring::afk_page(&ctx, community, come_back)?;
    let stage = div()
        .id("voice-stage")
        .relative()
        .size_full()
        .flex()
        .flex_col()
        .bg(color::BG)
        .child(stage_title(channel))
        .children(ring_notice(&ctx.state, channel))
        .child(
            div()
                .flex_1()
                .min_h_0()
                .child(tracked("voice-afk-page", page)),
        );
    Some(tracked("voice-stage-root", stage).into_any_element())
}

/// The stage's title bar: the channel's name.
fn stage_title(channel: &Channel) -> Div {
    div()
        .flex_none()
        .h(layout::BAR)
        .px(px(14.0))
        .flex()
        .items_center()
        .border_b_1()
        .border_color(color::HAIRLINE)
        .child(line(channel.name.clone(), text::MD, 18.0, color::FG).weight(font::WEIGHT_LABEL))
}

/// The ring area's stage notice for `channel` (VoiceView.tsx `stageNotice`): a block (Task 18's
/// `ring::block_notice`, with the comparison of the changed contact it names), else the line an AFK move left on the
/// stage of a voice channel of that server (`ring::afk_moved`).
pub fn ring_notice(state: &AppState, channel: &Channel) -> Option<AnyElement> {
    use crate::screens::ring::{afk_moved, block_notice};
    if let Some(n) = block_notice(state, channel.id, |u| {
        dilla_client::features::ring::name_of(state, u)
    }) {
        let record = n.compare.and_then(|u| {
            state
                .trust
                .contacts
                .iter()
                .find(|r| r.user_id == u)
                .cloned()
        });
        return Some(notice_strip("voice-blocked", n.text, record));
    }
    let afk_channel = state
        .ring
        .afk
        .get(&channel.community_id)
        .and_then(|s| s.channel);
    let moved = state
        .ring
        .afk_notice
        .as_ref()
        .filter(|n| n.community == channel.community_id && afk_channel != Some(channel.id))
        .and_then(|_| {
            afk_moved(state, |id| {
                state
                    .channel(id)
                    .map(|c| c.name.clone())
                    .unwrap_or_default()
            })
        })?;
    Some(notice_strip("voice-afk-moved", moved, None))
}

/// One stage notice: its line, and the comparison it offers (`voice.pausedAction`) when it names a changed contact.
fn notice_strip(
    id: &'static str,
    text: String,
    compare: Option<dilla_client::state::trust::TrustSummary>,
) -> AnyElement {
    let action = compare.map(|record| {
        tracked_box(
            "voice-notice-compare",
            dilla_desktop_ui::components::Button::new(
                "voice-notice-compare-b",
                t("voice.pausedAction"),
            )
            .on_click(move |_, _, cx| {
                let client = ui(cx).and_then(|u| u.client.clone());
                crate::screens::dms::compare::open(&record, client, cx);
            }),
        )
    });
    tracked_box(id, notice_box(text).children(action)).into_any_element()
}

/// Lets go of `client` (Forget, sign-out: `crate::release::release_client`): its handle and its call's media session go,
/// so the store can close.
pub fn release(client: &Rc<Client>, cx: &mut App) {
    if !cx.has_global::<VoiceUi>() {
        return;
    }
    let ui = cx.global_mut::<VoiceUi>();
    if ui.client.as_ref().is_some_and(|c| Rc::ptr_eq(c, client)) {
        ui.client = None;
        ui.session = None;
        ui.images.clear();
        ui.told_in_call = None;
        ui.told_roster = None;
    }
}

fn ui(cx: &App) -> Option<&VoiceUi> {
    cx.try_global::<VoiceUi>()
}

fn voice(cx: &App) -> Option<dilla_client::features::voice::Voice> {
    ui(cx)?.client.as_ref().map(|c| c.voice())
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

/// Joins `channel`'s call (the voice row's join, the stage's join).
pub fn join(channel: ChannelId, cx: &mut App) {
    if let Some(v) = voice(cx) {
        drop(v.join(channel));
    }
}

/// Leaves the call; the focus goes back to the call's voice row (leave-reload.spec.ts).
pub fn leave(cx: &mut App) {
    if let Some(v) = voice(cx) {
        drop(v.leave());
    }
}

pub fn toggle_mute(cx: &mut App) {
    if let (Some(v), Some(ui)) = (voice(cx), ui(cx)) {
        let s = &ui.state.voice;
        v.set_muted(!(s.self_mute || s.self_deaf));
    }
}

pub fn toggle_deafen(cx: &mut App) {
    if let (Some(v), Some(ui)) = (voice(cx), ui(cx)) {
        v.set_deafened(!ui.state.voice.self_deaf);
    }
}

pub fn toggle_share(kind: ShareKind, cx: &mut App) {
    let Some(ui) = ui(cx) else { return };
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
pub fn channel_people(channel: &Channel, _window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let state = ui(cx)?.state.clone();
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
            .pl(px(30.0))
            .pr(px(8.0))
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
        .border(px(2.0))
        .border_color(if p.speaking {
            color::BG_2
        } else {
            gpui::transparent_black()
        })
        .m(px(-2.0))
        .child(avatar);
    let ring = div()
        .flex_none()
        .rounded(radius::AVATAR)
        .border(px(1.5))
        .border_color(if p.speaking {
            color::OK
        } else {
            gpui::transparent_black()
        })
        .m(px(-1.5))
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
        .gap(px(8.0))
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
            d.child(Icon::new(IconName::Video).size(px(12.0)).color(color::FG_2))
        })
        .when_some(p.screen.then_some(()), |d, ()| d.child(live_tag()))
        .when_some(glyph, |d, g| {
            d.child(Icon::new(g).size(px(12.0)).color(color::DANGER))
        })
}

fn live_tag() -> Div {
    div()
        .flex_none()
        .px(px(4.0))
        .rounded(radius::TAG)
        .bg(color::DANGER)
        .child(line("LIVE", text::MICRO, 14.0, color::DANGER_INK).weight(font::WEIGHT_LABEL))
}

use gpui::prelude::FluentBuilder as _;

// ---- the dock -----------------------------------------------------------------------------------------------------

/// The voice dock over the user panel (`slots::dock`, first): the call's channel and its state, camera, share and
/// leave. Shown from the join's start to the leave.
pub fn dock(_window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let state = ui(cx)?.state.clone();
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
        .gap(px(2.0))
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
            .on_click(|_, _, cx| toggle_share(ShareKind::Camera, cx)),
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
            .on_click(|_, _, cx| toggle_share(ShareKind::Screen, cx)),
        ))
        .child(tracked_box(
            "voice-leave",
            IconButton::new("voice-leave-b", IconName::PhoneOff, t("voice.leave"))
                .size(IconButtonSize::S28)
                .on_click(|_, _, cx| leave(cx)),
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
        .gap(px(8.0))
        .mx(px(8.0))
        .mb(px(8.0))
        .p(px(10.0))
        .min_w_0()
        .bg(color::OK_SOFT)
        .border_1()
        .border_color(color::OK_SOFT_BORDER)
        .rounded(radius::LG)
        .child(
            div()
                .flex()
                .items_center()
                .gap(px(8.0))
                .min_w_0()
                .child(status)
                .child(controls),
        )
        .child(ptt_listener());
    Some(tracked_box("voice-dock", card).into_any_element())
}

/// Push to talk while the window is focused: the person's chord (`voice.pttChord`) held opens the microphone, its
/// release closes it. Listened for while a call is on (the dock is drawn).
fn ptt_listener() -> impl IntoElement {
    canvas(
        |_, _, _| {},
        |_, _, window, cx| {
            let chord = ui(cx)
                .and_then(|u| u.client.as_ref())
                .and_then(|c| c.prefs().get::<String>("voice.pttChord"));
            let Some(chord) = chord else { return };
            let key = keys::ptt_key(&chord);
            let down = key.clone();
            window.on_key_event(move |e: &gpui::KeyDownEvent, phase, _, cx| {
                if phase == gpui::DispatchPhase::Capture
                    && !e.is_held
                    && e.keystroke.key.eq_ignore_ascii_case(&down)
                    && let Some(v) = voice(cx)
                {
                    v.set_ptt_held(true);
                }
            });
            window.on_key_event(move |e: &gpui::KeyUpEvent, phase, _, cx| {
                if phase == gpui::DispatchPhase::Capture
                    && e.keystroke.key.eq_ignore_ascii_case(&key)
                    && let Some(v) = voice(cx)
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
    if let Some(page) = afk_stage(channel, cx) {
        return Some(page);
    }
    if ui(cx)?.stage_focus.is_none() {
        let handle = cx.focus_handle();
        cx.global_mut::<VoiceUi>().stage_focus = Some(handle);
    }
    let images = refresh_images(window, cx);
    let stats: HashMap<StreamKey, (u32, u32)> = ui(cx)
        .map(|u| {
            let videos = u.session.as_ref().map(|s| s.videos()).unwrap_or_default();
            u.rates
                .iter()
                .filter_map(|(k, (_, _, fps))| Some((k.clone(), (videos.get(k)?.height, *fps))))
                .collect()
        })
        .unwrap_or_default();
    let menu = ui(cx).and_then(share_menu);
    let ui = ui(cx)?;
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
            .gap(px(10.0))
            .child(
                div().flex_1().min_h_0().flex().justify_center().child(
                    div()
                        .h_full()
                        .aspect_ratio(16.0 / 9.0)
                        .max_w_full()
                        .child(main),
                ),
            )
            .child(
                div()
                    .flex()
                    .justify_center()
                    .gap(px(10.0))
                    .children(views.into_iter().map(|v| {
                        div()
                            .flex_none()
                            .w(px(188.0))
                            .aspect_ratio(16.0 / 9.0)
                            .child(v)
                    })),
            )
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
            .gap(px(10.0))
            .p(px(2.0))
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
    let title = stage_title(channel);
    let n = keys.len();
    let stage = div()
        .id("voice-stage")
        .relative()
        .track_focus(&focus)
        .size_full()
        .flex()
        .flex_col()
        .bg(color::BG)
        .on_key_down(move |e, window, cx| stage_key(&e.keystroke.key, n, window, cx))
        .child(title)
        .children(ring_notice(&state, channel))
        .child(notice(&state))
        .child(
            div()
                .flex_1()
                .min_h_0()
                .flex()
                .flex_col()
                .p(px(14.0))
                .child(body),
        )
        .child(controls(&state, joined, channel.id))
        .children(menu);
    let _ = window;
    Some(tracked("voice-stage-root", stage).into_any_element())
}

/// The stage's keys: the arrows move between tiles, Enter focuses the selected one, Escape leaves focus mode.
fn stage_key(key: &str, n: usize, _window: &mut Window, cx: &mut App) {
    if n == 0 || !cx.has_global::<VoiceUi>() {
        return;
    }
    let channel = cx.global::<VoiceUi>().state.voice.call.channel;
    let ui = cx.global_mut::<VoiceUi>();
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
        _ => return,
    }
    cx.refresh_windows();
}

/// The newest remote frames as images, rebuilt only for streams with a new frame.
/// A replaced or ended stream's image leaves the GPU atlas (`drop_image`): without that every frame would stay there.
fn refresh_images(window: &mut Window, cx: &mut App) -> HashMap<StreamKey, Arc<RenderImage>> {
    let Some(videos) = ui(cx).and_then(|u| u.session.as_ref()).map(|s| s.videos()) else {
        return HashMap::new();
    };
    let mut stale: Vec<Arc<RenderImage>> = Vec::new();
    let ui = cx.global_mut::<VoiceUi>();
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
        .left(px(8.0))
        .bottom(px(8.0))
        .flex()
        .items_center()
        .gap(px(6.0))
        .px(px(6.0))
        .rounded(radius::MD)
        .bg(color::BG_2)
        .child(line(name, text::XS, 14.0, color::FG).weight(font::WEIGHT_LABEL))
        .when(deafened, |d| {
            d.child(
                Icon::new(IconName::HeadphoneOff)
                    .size(px(12.0))
                    .color(color::DANGER),
            )
        })
        .when(muted && !deafened, |d| {
            d.child(
                Icon::new(IconName::MicOff)
                    .size(px(12.0))
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
            px(2.0)
        } else {
            px(1.0)
        })
        .border_color(if speaking {
            color::OK
        } else if selected {
            color::ACCENT
        } else {
            color::HAIRLINE
        })
        .cursor_pointer()
        .on_click(move |_, _, cx| {
            if cx.has_global::<VoiceUi>() {
                let ui = cx.global_mut::<VoiceUi>();
                ui.focused = if ui.focused.as_ref() == Some(&key) {
                    None
                } else {
                    Some(key.clone())
                };
                cx.refresh_windows();
            }
        })
        .child(media)
        .child(caption)
        .when_some(stat, |d, stat| {
            d.child(
                div()
                    .absolute()
                    .top(px(8.0))
                    .right(px(8.0))
                    .px(px(6.0))
                    .rounded(radius::MD)
                    .bg(color::BG_2)
                    .child(line(stat, text::MICRO, 14.0, color::FG_2)),
            )
        })
        .into_any_element()
}

/// A stage notice's box: the warning glyph and its line (the media notice and the ring area's notices).
fn notice_box(text: String) -> Div {
    div()
        .mx(px(14.0))
        .mt(px(14.0))
        .px(px(12.0))
        .py(px(10.0))
        .flex()
        .items_center()
        .gap(px(12.0))
        .bg(color::SURFACE_2)
        .border_1()
        .border_color(color::LINE_STRONG)
        .rounded(radius::MD)
        .child(
            Icon::new(IconName::TriangleAlert)
                .size(px(14.0))
                .color(color::WARN),
        )
        .child(div().flex_1().child(line(text, text::UI, 15.6, color::FG)))
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
    notice_box(t(key).to_owned())
        .id("voice-notice")
        .child(
            IconButton::new(
                "voice-notice-close",
                IconName::X,
                t("voice.unsupported.close"),
            )
            .size(IconButtonSize::S28)
            .on_click(|_, _, cx| {
                if let Some(v) = voice(cx) {
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
        .h(px(64.0))
        .flex()
        .items_center()
        .justify_center()
        .gap(px(6.0))
        .border_t_1()
        .border_color(color::HAIRLINE);
    if !joined {
        // Flow 06: no join button while a block holds the join back (Task 18's `join_held`).
        if crate::screens::ring::join_held(state, channel) {
            return bar.into_any_element();
        }
        let busy = state.voice.call.channel == Some(channel);
        return bar
            .child(tracked_box(
                "voice-join",
                IconButton::new("voice-join-b", IconName::Phone, t("voice.join"))
                    .size(IconButtonSize::S40)
                    .pressed(busy)
                    .on_click(move |_, _, cx| join(channel, cx)),
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
        .on_click(|_, _, cx| toggle_mute(cx)),
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
        .on_click(|_, _, cx| toggle_deafen(cx)),
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
        .on_click(|_, _, cx| toggle_share(ShareKind::Camera, cx)),
    ))
    .child(share_split(v.share.screen))
    .child(tracked_box(
        "voice-stage-leave",
        IconButton::new("voice-stage-leave-b", IconName::PhoneOff, t("voice.leave"))
            .size(IconButtonSize::S40)
            .on_click(|_, _, cx| leave(cx)),
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
            .on_click(|_, _, cx| toggle_share(ShareKind::Screen, cx)),
        ))
        .child(tracked_box(
            "voice-share-quality",
            IconButton::new(
                "voice-share-quality-b",
                IconName::ChevronUp,
                t("callStage.share.quality"),
            )
            .size(IconButtonSize::S28)
            .on_click(|_, _, cx| {
                if cx.has_global::<VoiceUi>() {
                    let ui = cx.global_mut::<VoiceUi>();
                    ui.share_menu = !ui.share_menu;
                    cx.refresh_windows();
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
    let pick = move |next: ShareOptions, cx: &mut App| {
        if !cx.has_global::<VoiceUi>() {
            return;
        }
        let ui = cx.global_mut::<VoiceUi>();
        ui.share_options = next;
        if ui.state.voice.share.screen
            && let Some(s) = &ui.session
        {
            s.quality(next);
        }
        cx.refresh_windows();
    };
    let label = |key: &'static str| line(t(key), text::XS, 14.0, color::FG_2);
    let panel = div()
        .id("voice-share-menu")
        .absolute()
        .bottom(px(72.0))
        .w(px(320.0))
        .p(px(12.0))
        .flex()
        .flex_col()
        .gap(px(8.0))
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
            .on_change(move |v, _, cx| {
                if let Some(resolution) = ShareResolution::from_name(v) {
                    pick(ShareOptions { resolution, ..o }, cx);
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
            .on_change(move |v, _, cx| {
                if let Ok(fps) = v.parse() {
                    pick(ShareOptions { fps, ..o }, cx);
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
                .on_click(|_, _, cx| {
                    if cx.has_global::<VoiceUi>() {
                        cx.global_mut::<VoiceUi>().share_menu = false;
                    }
                    toggle_share(ShareKind::Screen, cx);
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

//! Area **voice** (Task 17): the voice channel row's people, the dock, the stage and the call controls.
//!
//! Ports packages/ui `VoiceChannelRow` (the people), `VoiceDock`, `CallStage`, `CallTile`, `CallControls` and the
//! keyboard of packages/web/src/screens/voice/shortcuts.ts. The hooks below are where `crate::slots` draws this area
//! inside the shell; they read the voice slice through [`VoiceUi`], which the shell keeps current with [`sync`].
//!
//! The media of the call runs in [`session::MediaSession`] (one per process); the stage's video tiles draw its
//! newest remote frames.

pub mod keys;
pub mod menu;
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
    Window, WindowId, anchored, deferred, div, img, px, relative,
};

use dilla_desktop_ui::a11y::{Accessible, Role};
use dilla_desktop_ui::components::icon_button::{IconButton, IconButtonSize};
use dilla_desktop_ui::components::{Avatar, AvatarShape, AvatarSize};
use dilla_desktop_ui::metrics::focus_ring;
use dilla_desktop_ui::text::{Face, chromium_baseline, line, line_height};
use dilla_desktop_ui::tokens::{color, font, layout, radius, text};
use dilla_desktop_ui::{Icon, IconName};

use crate::strings::{t, tf};
use crate::{tracked, tracked_box};

use session::MediaSession;

/// What this client's microphone sends (`dilla_media`'s Opus encoder at 48 kHz): the own tile's codec stat.
const OWN_CODEC: (&str, &str) = ("opus", "48");

/// The regular density's line height (`--line-height`, 1.5).
const LINE: f32 = 1.5;
/// `.d-call-stage__where`'s line height.
const BAR_LINE: f32 = 1.3;

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
    /// CallTile.css: the caption 8px from the corner, 6px between its parts; the edge 1px,
    /// 2px while speaking or selected.
    pub const CAPTION_INSET: f32 = 8.0;
    pub const CAPTION_GAP: f32 = 6.0;
    pub const TILE_EDGE: f32 = 1.0;
    pub const TILE_EDGE_STRONG: f32 = 2.0;
    /// The media notice: 12x10 in, a 12px gap, the 14px warning glyph.
    pub const NOTICE_PAD_X: f32 = 12.0;
    pub const NOTICE_PAD_Y: f32 = 10.0;
    pub const NOTICE_GAP: f32 = 12.0;
    pub const NOTICE_GLYPH: f32 = 14.0;
    /// CallStage.css `.d-call-stage__controls[data-bar]`: the 64px bar (`--panel-h`), 6px between its parts, 10px by 16px in;
    /// `__room`: the 6px `--ok` dot 6px before the name.
    pub const CONTROLS_H: f32 = 64.0;
    pub const CONTROLS_GAP: f32 = 6.0;
    pub const BAR_PAD_X: f32 = 16.0;
    pub const BAR_PAD_Y: f32 = 10.0;
    pub const BAR_DOT: f32 = 6.0;
    pub const BAR_ROOM_GAP: f32 = 6.0;
    /// CallControls.css `.d-call-controls--bar`: 40px controls at least 44px wide, 12px in (Leave 14px, set 8px further
    /// apart), the glyph (16px), the word and the keycap 8px apart; the keycap at 0.6 (Signal's theme). Below a 1240px
    /// (77.5rem) window only the glyphs show (Leave keeps its word).
    pub const BAR_BUTTON_H: f32 = 40.0;
    pub const BAR_BUTTON_MIN_W: f32 = 44.0;
    pub const BAR_BUTTON_PAD_X: f32 = 12.0;
    pub const BAR_LEAVE_PAD_X: f32 = 14.0;
    pub const BAR_LEAVE_GAP: f32 = 8.0;
    pub const BAR_BUTTON_GAP: f32 = 8.0;
    pub const BAR_GLYPH: f32 = 16.0;
    pub const KEYCAP_OPACITY: f32 = 0.6;
    pub const BAR_WORDS_MIN_W: f32 = 1240.0;
    /// The live share's Stream quality chevron: 30 x 40, 2px after Share (`margin-left: calc(2px - 0.375rem)`), a 14px
    /// glyph, its left edge the 1px ink seam.
    pub const QUALITY_W: f32 = 30.0;
    pub const QUALITY_SEAM: f32 = 2.0 - CONTROLS_GAP;
    pub const QUALITY_GLYPH: f32 = 14.0;
    pub const SEAM: f32 = 1.0;
    /// `.d-call-stage__preview`: 10px in, 16px between the faces, the text and Join voice, 4px between the text's lines;
    /// `__face`: 48px faces 10px over the one before, a 3px `--bg` ring and a 5px `--ok` ring on whoever speaks;
    /// `__join`: 40px, 18px in, the 15px phone glyph 8px before the word.
    pub const PREVIEW_PAD: f32 = 10.0;
    pub const PREVIEW_GAP: f32 = 16.0;
    pub const PREVIEW_TEXT_GAP: f32 = 4.0;
    pub const FACE: f32 = 48.0;
    pub const FACE_OVERLAP: f32 = 10.0;
    pub const FACE_RING: f32 = 3.0;
    pub const FACE_SPEAKING: f32 = 5.0;
    pub const JOIN_H: f32 = 40.0;
    pub const JOIN_PAD_X: f32 = 18.0;
    pub const JOIN_GAP: f32 = 8.0;
    pub const JOIN_GLYPH: f32 = 15.0;
    /// CallTile.css `[data-stage] .d-call-tile__caption`: a 22px chip 8px in from the bottom left, 7px in; `__stat`: 8px from
    /// the right and 7px from the top.
    pub const CAPTION_H: f32 = 22.0;
    /// `.d-call-tile__meter`: three 2 x 10px bars 1.5px apart.
    pub const METER_W: f32 = 2.0;
    pub const METER_H: f32 = 10.0;
    pub const METER_GAP: f32 = 1.5;
    pub const CAPTION_CHIP_PAD_X: f32 = 7.0;
    pub const STAT_RIGHT: f32 = 8.0;
    /// `.d-call-tile__tools`: 10px in from the focused tile's top right (its stat 10px in from the bottom right), 4px
    /// apart; each tool 28px; Grid 9px in, its 13px glyph 6px before the word; full screen's glyph 14px.
    pub const TOOLS_INSET: f32 = 10.0;
    pub const TOOLS_GAP: f32 = 4.0;
    pub const TOOL: f32 = 28.0;
    pub const GRID_TOOL_PAD_X: f32 = 9.0;
    pub const GRID_TOOL_GAP: f32 = 6.0;
    pub const GRID_GLYPH: f32 = 13.0;
    pub const FULLSCREEN_GLYPH: f32 = 14.0;
    pub const STAT_TOP: f32 = 7.0;
    /// The stream-quality menu: 320px wide, 72px over the stage's foot (the bar and its gap), 12px in, 8px gaps.
    pub const MENU_BOTTOM: f32 = 72.0;
    pub const MENU_W: f32 = 320.0;
    pub const MENU_PAD: f32 = 12.0;
    pub const MENU_GAP: f32 = 8.0;
    /// ChannelHeader.css: the bar 20px in on the left and 10px on the right, 10px between title and topic, the 14px
    /// glyph 6px before the name.
    pub const HEADER_LEFT: f32 = 20.0;
    pub const HEADER_RIGHT: f32 = 10.0;
    pub const HEADER_GAP: f32 = 10.0;
    pub const HEADER_TITLE_GAP: f32 = 6.0;
    pub const HEADER_GLYPH: f32 = 14.0;
    /// app.css `.dw-dm-call`: the DM call box is at most 40 % of the conversation's height; its grid's columns are at
    /// least 11rem wide (`minmax(min(100%, 11rem), 1fr)`), its cards 8px in with no least height.
    pub const DM_BOX_SHARE: f32 = 0.4;
    pub const DM_TILE_MIN: f32 = 176.0;
    pub const DM_TILE_PAD: f32 = 8.0;
    /// CallTile.css, the compact card: 6px between the media and the caption, the caption at least 1.25rem high.
    pub const CARD_GAP: f32 = 6.0;
    pub const CAPTION_MIN_H: f32 = 20.0;
    /// CallStage.css `.d-call-stage__head`: 8px between its rows, 16px between its parts.
    pub const HEAD_GAP_Y: f32 = 8.0;
    pub const HEAD_GAP_X: f32 = 16.0;
    /// `.d-call-stage__summary`: the call code's 28px disclosure, 8px in, its chevron 0.875em, 6px before the word.
    pub const CODE_H: f32 = 28.0;
    pub const CODE_PAD_X: f32 = 8.0;
    pub const CODE_GLYPH_EM: f32 = 0.875;
    pub const CODE_GLYPH_GAP: f32 = 6.0;
    /// `.d-call-stage__code-body`: at most 26rem, 6px under the summary, 8px by 12px in; its help 6px under the digits.
    pub const CODE_BODY_MAX_W: f32 = 416.0;
    pub const CODE_BODY_GAP: f32 = 6.0;
    pub const CODE_BODY_PAD_Y: f32 = 8.0;
    pub const CODE_BODY_PAD_X: f32 = 12.0;
    pub const CODE_HELP_GAP: f32 = 6.0;
    /// app.css: the DM box's controls stand 8px above and below; CallControls.css: 2px between the toolbar's buttons,
    /// and the hairline before leave 8px high in and 4px aside.
    pub const DM_CONTROLS_PAD_Y: f32 = 8.0;
    pub const TOOLBAR_GAP: f32 = 2.0;
    /// `.d-voice-toolbar__control`: 8px in; a pressed control's underline 2px.
    pub const TOOLBAR_PAD_X: f32 = 8.0;
    pub const TOOLBAR_UNDERLINE: f32 = 2.0;
    pub const DIVIDER_INSET_Y: f32 = 8.0;
    pub const DIVIDER_INSET_X: f32 = 4.0;
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
    /// The AFK page's Come back: this window's shell's way to a text channel the person can send in ([`set_come_back`]).
    come_back: Option<crate::screens::ring::OnComeBack>,
    /// The push-to-talk key (Task 19's `PttHold`): held, or let go less than the release delay ago.
    ptt: dilla_client::features::voice_prefs::ptt::PttHold,
    /// Push to talk's key (as GPUI names it) and release delay, from the settings ([`set_ptt_key`]).
    ptt_key: Option<(String, u32)>,
    /// The DM call box's tile grid's width, logical px, as last laid out ([`dm_call_box`]): its column count follows it.
    dm_grid_w: f32,
    /// The tile shown full screen ([`toggle_tile_fullscreen`]).
    pub fullscreen: Option<TileFullscreen>,
    /// The stage's person menu ([`menu`]).
    pub menu: menu::MenuState,
    /// The shell's way to open a voice channel (Move to), set as it draws ([`set_open_voice`]).
    open_voice: Option<OpenVoice>,
    /// The DM box's call code disclosure is open (CallStage.tsx's `<details>`), and its summary's focus.
    pub code_open: bool,
    code_focus: Option<FocusHandle>,
    /// A test's remote frames, drawn while there is no media session (feature `test-hooks`).
    #[cfg(feature = "test-hooks")]
    test_frames: HashMap<StreamKey, Arc<RenderImage>>,
}

/// Every instance window's voice area, by window. A window that closes is let go at once (`windows::on_closed`: its
/// call left, its media session and client dropped).
#[derive(Default)]
pub struct VoiceAreas {
    windows: HashMap<WindowId, VoiceUi>,
    /// The repaint loops running now, one per live call ([`repaint_on_video`]).
    loops: usize,
    /// The newest join's [`handover`] (review L m1): one call per device, so an older handover still waiting starts no
    /// join of its own.
    handovers: u64,
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
    release_ptt(window, cx);
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

/// A tile filling its window (CallTile.tsx `toggleFullscreen`: the web's element full screen): which, whether the window
/// went full screen for it ([`crate::fullscreen`]), and the focus that takes Escape.
pub struct TileFullscreen {
    pub key: TileKey,
    made_window: bool,
    focus: FocusHandle,
}

/// The tile's full-screen action: `key` fills the window (the window full screen, only the tile drawn), or, shown
/// already, goes back to the stage.
pub fn toggle_tile_fullscreen(key: TileKey, window: &mut Window, cx: &mut App) {
    let w = wid(window);
    if ui_of(w, cx).is_none() {
        return;
    }
    if ui_of(w, cx)
        .and_then(|u| u.fullscreen.as_ref())
        .is_some_and(|f| f.key == key)
    {
        exit_tile_fullscreen(w, cx);
        return;
    }
    exit_tile_fullscreen(w, cx);
    let made_window = crate::fullscreen::enter(window);
    let focus = cx.focus_handle();
    window.focus(&focus, cx);
    let ui = ui_mut(w, cx);
    ui.focused = Some(key.clone());
    ui.fullscreen = Some(TileFullscreen {
        key,
        made_window,
        focus,
    });
    window.refresh();
}

/// The full-screen tile goes back to the stage (Escape, its button, the stream gone, the call ended), and the window out
/// of full screen if it went in for the tile.
pub fn exit_tile_fullscreen(w: WindowId, cx: &mut App) {
    let Some(f) = ui_of(w, cx)
        .is_some()
        .then(|| ui_mut(w, cx).fullscreen.take())
        .flatten()
    else {
        return;
    };
    crate::fullscreen::leave(w, f.made_window, cx);
    cx.refresh_windows();
}

/// The tile `w` shows full screen, if any.
pub fn fullscreen_tile(w: WindowId, cx: &App) -> Option<TileKey> {
    ui_of(w, cx)?.fullscreen.as_ref().map(|f| f.key.clone())
}

/// Lets go of `client` (Forget, sign-out: `crate::release::release_client`): every window area that holds it goes,
/// with its media session (the call was left by the caller first; this is not `windows::on_closed`, which leaves it).
pub fn release(client: &Rc<Client>, cx: &mut App) {
    if !cx.has_global::<VoiceAreas>() {
        return;
    }
    let_go(cx, |_, ui| {
        ui.client.as_ref().is_some_and(|c| Rc::ptr_eq(c, client))
    });
}

/// Test hook: `window`'s voice area is let go as a released client's is.
#[cfg(feature = "test-hooks")]
pub fn window_released_for_tests(window: WindowId, cx: &mut App) {
    let_go(cx, |w, _| *w == window);
}

/// Lets go of the voice areas `gone` picks; a tile one showed full screen gives its window back out of full screen.
fn let_go(cx: &mut App, gone: impl Fn(&WindowId, &VoiceUi) -> bool) {
    if !cx.has_global::<VoiceAreas>() {
        return;
    }
    let mut restore = Vec::new();
    cx.global_mut::<VoiceAreas>().windows.retain(|w, ui| {
        let keep = !gone(w, ui);
        // A tile shown full screen goes with its client: the window comes back out of full screen.
        if !keep && let Some(f) = ui.fullscreen.take() {
            restore.push((*w, f.made_window));
        }
        keep
    });
    for (w, made) in restore {
        crate::fullscreen::leave(w, made, cx);
    }
}

/// The shell's way to open a voice channel's stage (Shell.tsx `openVoice`).
pub type OpenVoice = Rc<dyn Fn(ChannelId, &mut App)>;

/// `window`'s shell's way to open a voice channel, set on each render of the shell.
pub fn set_open_voice(window: WindowId, open: OpenVoice, cx: &mut App) {
    ui_mut(window, cx).open_voice = Some(open);
}

/// Opens `channel`'s stage in `window` (the person menu's Move to), through its shell.
pub fn open_voice(window: WindowId, channel: ChannelId, cx: &mut App) {
    if let Some(open) = ui_of(window, cx).and_then(|u| u.open_voice.clone()) {
        open(channel, cx);
    }
}

/// `window`'s shell's way out of the AFK page (Shell.tsx `onLeaveAfk`), set on each render of the shell.
pub fn set_come_back(window: WindowId, come_back: crate::screens::ring::OnComeBack, cx: &mut App) {
    ui_mut(window, cx).come_back = Some(come_back);
}

/// The slot context of the stage's channel, from what `ui` holds (the ring area's stage reads take one).
fn stage_ctx(ui: &VoiceUi, channel: &Channel) -> crate::slots::SlotCx {
    crate::slots::SlotCx {
        state: ui.state.clone(),
        client: ui.client.clone(),
        community: Some(channel.community_id),
        channel: Some(channel.id),
        clock: crate::screens::shell::Clock::Local,
        workspace: None,
    }
}

/// The AFK channel's page (VoiceView.tsx `AfkPage`), in place of the stage, when `channel` is its server's AFK channel:
/// the stage's title bar, the stage notice and the ring area's page (Task 18: who is away, the server's timeout, Go AFK
/// or Come back). A server whose AFK snapshot is not read yet is asked for it (`Ring::load_afk` reads one at a time) and
/// shows the ordinary stage meanwhile.
fn afk_stage(channel: &Channel, w: WindowId, cx: &mut App) -> Option<AnyElement> {
    let ui = ui_of(w, cx)?;
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
        .a11y_unnamed(Role::Region)
        .relative()
        .size_full()
        .flex()
        .flex_col()
        .bg(color::BG)
        .child(stage_title(
            channel,
            IconName::Moon,
            t("voice.header.afk").to_owned(),
        ))
        .children(ring_notice(&ctx.state, channel))
        .child(div().flex_1().min_h_0().child(tracked(
            "voice-afk-page",
            // `.d-call-stage__afk` fills the stage and centres its column.
            div().size_full().flex().flex_col().child(page),
        )));
    Some(tracked("voice-stage-root", stage).into_any_element())
}

/// The stage's title bar (VoiceView.tsx: the voice channel's `ChannelHeader`, no controls): the 14px glyph, the name in
/// 13.5px/700 and the topic in 12px --fg-2, on the 44px bar with its hairline.
fn stage_title(channel: &Channel, glyph: IconName, topic: String) -> Div {
    div()
        .flex_none()
        .h(layout::CHANNEL_HEADER_H)
        .pl(px(m::HEADER_LEFT))
        .pr(px(m::HEADER_RIGHT))
        .flex()
        .items_center()
        .gap(px(m::HEADER_GAP))
        .bg(color::BG)
        .border_b_1()
        .border_color(color::HAIRLINE)
        .child(
            div()
                .flex_none()
                .flex()
                .items_center()
                .gap(px(m::HEADER_TITLE_GAP))
                .child(
                    Icon::new(glyph)
                        .size(px(m::HEADER_GLYPH))
                        .color(color::FG_3),
                )
                .child(
                    line(
                        channel.name.clone(),
                        text::BASE,
                        dilla_desktop_ui::text::line_height(text::BASE, 1.5),
                        color::FG,
                    )
                    .weight(font::WEIGHT_DISPLAY)
                    .ellipsis(),
                ),
        )
        .child(
            div().flex_1().min_w_0().overflow_hidden().child(
                line(
                    topic,
                    text::UI,
                    dilla_desktop_ui::text::line_height(text::UI, 1.5),
                    color::FG_2,
                )
                .ellipsis(),
            ),
        )
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
            .on_click(move |_, window, cx| {
                let client = client_of(wid(window), cx);
                crate::screens::dms::compare::open(wid(window), &record, client, cx);
            }),
        )
    });
    tracked_box(id, notice_box(text).children(action)).into_any_element()
}

impl VoiceUi {
    /// The stage's focus handle, once the stage has drawn (the arrows and Escape act while it holds the focus).
    pub fn stage_focus_handle(&self) -> Option<FocusHandle> {
        self.stage_focus.clone()
    }

    /// Test hook: `image` is the newest frame of `stream`, drawn while this window has no media session.
    #[cfg(feature = "test-hooks")]
    pub fn put_test_frame(&mut self, stream: StreamKey, image: Arc<RenderImage>) {
        self.test_frames.insert(stream, image);
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
        let host = cx
            .try_global::<AudioHostChoice>()
            .map_or_else(dilla_media::devices::system, |h| h.0.clone());
        let session = MediaSession::start_on(client, session::default_sources(), host)
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
        let v = dilla_client::features::voice_prefs::values::VoiceValues::read(&client.prefs());
        let key = v
            .ptt_chord
            .map(|chord| (keys::ptt_key(&chord), v.ptt_release_ms));
        set_ptt_key(window, key, cx);
    }
    if ui_mut(window, cx).state.voice.call.phase != CallPhase::Idle
        && state.voice.call.phase == CallPhase::Idle
    {
        // The call ended: push to talk lets go (review I1).
        release_ptt(window, cx);
    }
    if ui_mut(window, cx).state.voice.call.phase != CallPhase::Idle
        && state.voice.call.phase == CallPhase::Idle
    {
        // The call ended: a tile shown full screen goes, and the window with it.
        exit_tile_fullscreen(window, cx);
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

/// The audio host a call's media opens its devices on: the system's unless a test chose another.
struct AudioHostChoice(Arc<dyn dilla_media::devices::AudioHost>);

impl Global for AudioHostChoice {}

/// Test hook: the media sessions started from here on open their devices on `host` (a fake with no speaker, say).
#[cfg(feature = "test-hooks")]
pub fn use_audio_host(host: Arc<dyn dilla_media::devices::AudioHost>, cx: &mut App) {
    cx.set_global(AudioHostChoice(host));
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

/// The plain name of where this device's call is (Shell.tsx `whereOf`: the voice channel's or the DM's name) while it
/// is joining or in a call, `None` outside one: the top bar's voice chip.
pub fn call_room(state: &AppState) -> Option<String> {
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

/// Where this device's call is (Shell.tsx `callWhere`, product walk M8): a voice channel by its name, a DM's call as
/// "the call with {name}"; `None` outside a call. The ring's busy text, the dock and the user panel say it.
pub fn call_place(state: &AppState) -> Option<String> {
    let room = call_room(state)?;
    let dm = state
        .voice
        .call
        .channel
        .is_some_and(|c| state.channel(c).is_none() && state.dms.get(c).is_some());
    Some(if dm {
        tf("call.where", &[("name", &room)])
    } else {
        room
    })
}

/// Where this device's call is once it is up (Shell.tsx `selfRoom`: the call slice's phase `in`): the user panel's
/// "in {channel}".
pub fn in_room(state: &AppState) -> Option<String> {
    (state.voice.call.phase == CallPhase::In)
        .then(|| call_place(state))
        .flatten()
}

/// The route that opens this device's call (TopBar.tsx's voice chip): its voice channel, or its DM.
pub fn call_route(state: &AppState) -> Option<crate::router::Route> {
    let call = &state.voice.call;
    if call.phase == CallPhase::Idle {
        return None;
    }
    let id = call.channel?;
    match state.channel(id) {
        Some(c) => Some(crate::router::Route::Channel {
            community: c.community_id,
            channel: Some(id),
        }),
        None => state.dms.get(id).map(|_| crate::router::Route::Dm(id)),
    }
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
    // A person on two devices counts once, wherever the roster lists the second.
    users.sort_unstable();
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
    for c in other_calls(window, cx) {
        drop(c.voice().leave());
    }
}

/// How long a join waits for another instance window's leave to answer (Task 28 item 1). Past it the join is dropped
/// and the window says it could not join: a leave that never answers must not hold the join forever.
pub const HANDOVER_WAIT: Duration = Duration::from_secs(10);

/// How long the handover waits for the join's own answer (review L m2). Past it the handover stops looking: the window's
/// voice area still shows whatever the client's call becomes, and a window that closes leaves it then.
pub const JOIN_WAIT: Duration = Duration::from_secs(120);

/// How often the handover looks at the pending leaves.
const HANDOVER_POLL: Duration = Duration::from_millis(50);

/// An answer of the client's voice area (a leave, a join).
pub type VoiceAnswer = tokio::sync::oneshot::Receiver<Result<(), dilla_client::ClientError>>;

/// Leaves the call of every other instance window, then joins through `voice` once each leave has answered. A join
/// started while another instance's leave is still committing races it in the call group (`E_COMMIT_INVALID`), so the
/// join waits, for [`HANDOVER_WAIT`] at most. `start` starts the join (`Voice::join`, `Voice::join_dm`).
pub fn after_leaving_other_calls(
    window: WindowId,
    voice: dilla_client::features::voice::Voice,
    start: impl FnOnce(&dilla_client::features::voice::Voice) -> VoiceAnswer + 'static,
    cx: &mut App,
) {
    let leaves: Vec<_> = other_calls(window, cx)
        .into_iter()
        .map(|c| c.voice().leave())
        .collect();
    let undo = voice.clone();
    handover(
        window,
        leaves,
        move || start(&voice),
        move || drop(undo.leave()),
        cx,
    );
}

/// The handover of [`after_leaving_other_calls`]: waits for `leaves` (each answers on its client's runtime; the wait
/// polls them on the UI's executor, never blocking it) for [`HANDOVER_WAIT`] at most, then runs `start`. A leave that
/// does not answer in time shows `voice.failed` in `window`; a join that answers with an error shows its
/// [`join_failure_key`] notice (none for a cancelled start or a trust refusal).
///
/// Task 28 item 2: the join belongs to `window`'s voice area. A window that closed while the leaves were pending starts
/// no join; a join that lands after its window closed is left again (`undo`), else the client would stay in a call no
/// voice area shows, its microphone closed.
pub fn handover(
    window: WindowId,
    mut leaves: Vec<VoiceAnswer>,
    start: impl FnOnce() -> VoiceAnswer + 'static,
    undo: impl FnOnce() + 'static,
    cx: &mut App,
) {
    let ticket = {
        let areas = cx.default_global::<VoiceAreas>();
        areas.handovers += 1;
        areas.handovers
    };
    let newest = move |cx: &App| cx.try_global::<VoiceAreas>().map(|a| a.handovers) == Some(ticket);
    cx.spawn(async move |cx| {
        let mut waited = Duration::ZERO;
        loop {
            leaves.retain_mut(|l| {
                matches!(
                    l.try_recv(),
                    Err(tokio::sync::oneshot::error::TryRecvError::Empty)
                )
            });
            if leaves.is_empty() {
                break;
            }
            if !cx.update(|cx| newest(cx)) {
                return;
            }
            if waited >= HANDOVER_WAIT {
                log::warn!(
                    "voice: another instance's call did not answer its leave; the join is dropped"
                );
                cx.update(|cx| join_failed(window, cx));
                return;
            }
            cx.background_executor().timer(HANDOVER_POLL).await;
            waited += HANDOVER_POLL;
        }
        // A newer join started meanwhile (in this window or another): it is the device's one call (review L m1).
        if !cx.update(|cx| area_open(window, cx) && newest(cx)) {
            return;
        }
        // The join answers on the client's runtime; it is polled on the same timer, for [`JOIN_WAIT`] at most. (The
        // platform dispatcher would wake an await on the answer from another thread, but the tests' scheduler runs only
        // what its own clock and turns wake, so the poll keeps both alike.)
        let mut join = start();
        let mut joining = Duration::ZERO;
        // `None`: joined; else the notice to show (shell-model.ts `joinFailureKey`), if any.
        let failure: Option<Option<&'static str>> = loop {
            match join.try_recv() {
                Err(tokio::sync::oneshot::error::TryRecvError::Empty) => {
                    if joining >= JOIN_WAIT {
                        log::warn!("voice: a join did not answer; its window stops waiting for it");
                        return;
                    }
                    cx.background_executor().timer(HANDOVER_POLL).await;
                    joining += HANDOVER_POLL;
                }
                Err(tokio::sync::oneshot::error::TryRecvError::Closed) => {
                    break Some(Some("voice.failed"));
                }
                Ok(Ok(())) => break None,
                Ok(Err(e)) => break Some(join_failure_key(&e)),
            }
        };
        cx.update(|cx| {
            if !area_open(window, cx) {
                undo();
            } else if let Some(Some(key)) = failure {
                join_notice(window, key, cx);
            }
        });
    })
    .detach();
}

/// Whether `window` is open and its voice area is live (`window_closed` has not let it go).
fn area_open(window: WindowId, cx: &App) -> bool {
    ui_of(window, cx).is_some() && cx.windows().iter().any(|w| w.window_id() == window)
}

/// How long quitting waits for the live calls' leaves (Task 28 item 3).
pub const QUIT_WAIT: Duration = Duration::from_secs(3);

/// Task 28 item 3: quitting leaves every live call (its voice Remove reaches the instance, as a closed window's does),
/// waiting for the leaves [`QUIT_WAIT`] at most. The leaves answer on their clients' runtimes, so the UI thread may
/// block on them here. Answers whether every leave answered Ok in time.
pub fn on_quit(cx: &mut App) -> bool {
    let mut clients: Vec<Rc<Client>> = Vec::new();
    if let Some(areas) = cx.try_global::<VoiceAreas>() {
        for c in areas
            .windows
            .values()
            .filter(|u| in_a_call(u))
            .filter_map(|u| u.client.clone())
        {
            if !clients.iter().any(|k| Rc::ptr_eq(k, &c)) {
                clients.push(c);
            }
        }
    }
    let leaves: Vec<_> = clients.iter().map(|c| c.voice().leave()).collect();
    let answered = wait_answers(leaves, QUIT_WAIT);
    if !answered {
        log::warn!("voice: a call's leave failed or did not answer before the app quit");
    }
    answered
}

/// Waits for every answer in `answers` (an answer, or its sender gone) for `within` at most, blocking the calling
/// thread. Answers whether all answered `Ok` in time (review L I3: a leave answers `Ok` only once the instance
/// acknowledged its voice Remove; an error, or a sender gone, is no success).
pub fn wait_answers(mut answers: Vec<VoiceAnswer>, within: Duration) -> bool {
    let deadline = std::time::Instant::now() + within;
    let mut all_ok = true;
    loop {
        answers.retain_mut(|a| match a.try_recv() {
            Err(tokio::sync::oneshot::error::TryRecvError::Empty) => true,
            Ok(Ok(())) => false,
            Ok(Err(_)) | Err(tokio::sync::oneshot::error::TryRecvError::Closed) => {
                all_ok = false;
                false
            }
        });
        if answers.is_empty() {
            return all_ok;
        }
        if std::time::Instant::now() >= deadline {
            return false;
        }
        std::thread::sleep(Duration::from_millis(5));
    }
}

/// The notice for a join that answered `e` (shell-model.ts `joinFailureKey`), `None` when nothing is said.
pub fn join_failure_key(e: &dilla_client::ClientError) -> Option<&'static str> {
    let dilla_client::ClientError::Sync(code) = e else {
        return Some("voice.failed");
    };
    match code.as_str() {
        "E_CALL_FULL" => Some("voice.full"),
        "E_CALL_SHARERS_FULL" => Some("voice.sharersFull"),
        "E_CALL_IN_OTHER_TAB" => Some("voice.otherTab"),
        // A start the person cancelled (a leave, or another join, while joining: the client's `E_CANCELLED`, the web's
        // `E_ABORTED`) says nothing.
        "E_CANCELLED" | "E_ABORTED" => None,
        // A trust refusal sets the call's blocked notice, which says why (flow 06).
        "E_UMK_CHANGED" | "E_CREDENTIAL" | "E_DEVICE_UNLISTED" | "E_DEVICE_REVOKED"
        | "E_TIER_MISMATCH" | "E_BINDING" => None,
        _ => Some("voice.failed"),
    }
}

/// A join that did not happen: `window` says so (VoiceView.tsx's `voice.failed`).
fn join_failed(window: WindowId, cx: &mut App) {
    join_notice(window, "voice.failed", cx);
}

/// Shows the join notice `key` in `window`.
fn join_notice(window: WindowId, key: &'static str, cx: &mut App) {
    dilla_desktop_ui::components::toast::show_toast_in(Some(window), t(key), cx);
}

/// Joins `channel`'s call through `window`'s own client (the voice row's join, the stage's join), once a call another
/// instance window is in has been left.
pub fn join(channel: ChannelId, window: WindowId, cx: &mut App) {
    let Some(v) = voice(window, cx) else { return };
    after_leaving_other_calls(window, v, move |v| v.join(channel), cx);
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
        // VoiceChannelRow.tsx: a bot's row says "playback only" after the name (its rounded-square avatar marks it).
        .when(p.bot, |d| {
            d.child(
                div()
                    .flex_none()
                    .debug_selector(|| "voice-person-playback-only".into())
                    .child(dilla_desktop_ui::components::tag::Tag::new(
                        ElementId::Name(format!("voice-person-tag-{}", p.user.hex()).into()),
                        dilla_desktop_ui::components::tag::TagKind::PlaybackOnly,
                    )),
            )
        })
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

/// The link's line (VoiceDock.tsx's line 2, VoiceView.tsx `StatsLine`): the ping, then what this device sends and receives
/// ("24 ms · ↑ 38 ↓ 71 kb/s") once a rate was read.
pub fn link_text(ms: Option<u32>, rate: Option<(u32, u32)>) -> String {
    let ping = ping_text(ms);
    match rate {
        Some((up, down)) => format!(
            "{ping} · {}",
            tf(
                "voice.dock.rate",
                &[("up", &up.to_string()), ("down", &down.to_string())]
            )
        ),
        None => ping,
    }
}

/// VoiceView.tsx `Dock`'s ping of the link (`call.ping_ms`, the media's read): "— ms" until one is read (a parity
/// fixture's call has none), "<1 ms" under a millisecond, else "{ms} ms".
pub fn ping_text(ms: Option<u32>) -> String {
    match ms {
        None => t("voice.dock.pingNone").to_owned(),
        Some(0) => t("voice.dock.pingSub").to_owned(),
        Some(ms) => tf("voice.dock.ping", &[("ms", &ms.to_string())]),
    }
}

/// The voice dock over the user panel (`slots::dock`, first): the call's channel, its state and leave. Shown from the
/// join's start to the leave.
pub fn dock(window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let w = wid(window);
    let state = ui_of(w, cx)?.state.clone();
    let call = &state.voice.call;
    if call.phase == CallPhase::Idle {
        return None;
    }
    // VoiceView.tsx `Dock`'s `where`: the channel, or "the call with {name}" for a DM.
    let name = call_place(&state).unwrap_or_default();
    let where_ = match call.phase {
        CallPhase::In => link_text(call.ping_ms, call.rate_kbps),
        CallPhase::WaitingForRemove => t("voice.announce.rejoining").to_owned(),
        _ => t("voice.dock.connecting").to_owned(),
    };
    // VoiceView.tsx `Dock`: camera and share are the stage's controls; the dock holds leave only.
    let controls = div()
        .flex()
        .flex_none()
        .items_center()
        .gap(px(m::DOCK_CONTROLS_GAP))
        .child(tracked_box(
            "voice-leave",
            IconButton::new("voice-leave-b", IconName::PhoneOff, t("voice.leave"))
                .size(IconButtonSize::S28)
                .danger(true)
                .on_click(|_, window, cx| leave(wid(window), cx)),
        ));
    let status = div()
        .flex()
        .flex_col()
        .flex_1()
        .min_w_0()
        .child(
            line(name, text::UI, line_height(text::UI, 1.3), color::OK_TEXT)
                .weight(font::WEIGHT_LABEL),
        )
        .child(line(
            where_,
            text::META,
            line_height(text::META, 1.3),
            color::FG_3,
        ));
    // VoiceDock.tsx: `role="region" aria-label="voice controls"`.
    let card = div()
        .id("voice-dock-region")
        .landmark(Role::Region, t("voice.dock.region"))
        .flex()
        .flex_col()
        .gap(px(m::DOCK_GAP))
        .mx(px(m::DOCK_INSET))
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
        );
    // `.d-voice-dock`'s 8px bottom margin collapses through `.dw-voice-dock` (no padding or border below): the band under
    // the card is the shell's `--bg`, not the wrapper's `--bg-2`.
    Some(
        div()
            .flex()
            .flex_col()
            .child(tracked_box("voice-dock", card))
            .child(div().h(px(m::DOCK_INSET)).bg(color::BG))
            .into_any_element(),
    )
}

/// Milliseconds on one clock for the push-to-talk hold (its release delay is measured on it).
fn ptt_now_ms() -> f64 {
    // App-wide by design (final review sweep): the origin of one monotonic clock; it holds nothing else.
    static START: std::sync::OnceLock<std::time::Instant> = std::sync::OnceLock::new();
    START
        .get_or_init(std::time::Instant::now)
        .elapsed()
        .as_secs_f64()
        * 1000.0
}

/// Push to talk's key and its release delay for `window` (`None`: no key set). The shell's sync keeps it from Task 19's
/// validated values (`voice.pttChord`, `voice.pttRelease`).
pub fn set_ptt_key(window: WindowId, key: Option<(String, u32)>, cx: &mut App) {
    ui_mut(window, cx).ptt_key = key;
}

/// Whether push to talk sends from `window` now (the key held, or let go less than the release delay ago).
pub fn ptt_sending(window: WindowId, cx: &App) -> bool {
    ui_of(window, cx).is_some_and(|u| {
        let release = u.ptt_key.as_ref().map_or(0, |(_, ms)| *ms);
        u.ptt.sending(ptt_now_ms(), release)
    })
}

/// Push to talk lets go, and while the call is on the root takes the focus (so the next key reaches the root's
/// handlers).
fn take_back(w: WindowId, root: &FocusHandle, window: &mut Window, cx: &mut App) {
    release_ptt(w, cx);
    if call_live(w, cx) {
        window.focus(root, cx);
    }
}

/// Review B, at render: nothing holds the focus (a key would reach none of the root's handlers): see [`take_back`].
/// Re-review B2: a focus outside the drawn tree is never judged here, since a control mounted and focused in this very
/// update is not drawn yet; [`focus_lost`] covers a control that went away.
pub fn guard_focus(w: WindowId, root: &FocusHandle, window: &mut Window, cx: &mut App) {
    if window.focused(cx).is_none() {
        take_back(w, root, window, cx);
    }
}

/// Review B, after a frame is drawn: the focused control went away (GPUI's focus-lost, which the window's root
/// subscribes to with `on_focus_lost`): a key-up would be lost and the mic stay open; see [`take_back`].
pub fn focus_lost(w: WindowId, root: &FocusHandle, window: &mut Window, cx: &mut App) {
    take_back(w, root, window, cx);
}

/// Review B: the window stopped being the active one (it lost the focus, or was hidden): push to talk lets go at once.
/// The app calls it from the window's activation observer, which needs no frame (a hidden Wayland surface gets none).
pub fn window_deactivated(w: WindowId, cx: &mut App) {
    release_ptt(w, cx);
}

/// Whether `window`'s call is on (joining or in it).
pub fn call_live(window: WindowId, cx: &App) -> bool {
    ui_of(window, cx).is_some_and(in_a_call)
}

/// Lets go of push to talk in `window` at once (review I1): the screen left the shell, the window lost the focus or
/// closed, or the call ended. The microphone closes; idempotent.
pub fn release_ptt(window: WindowId, cx: &mut App) {
    let Some(ui) = ui_of(window, cx) else { return };
    let held = ui.ptt.is_down() || ptt_sending(window, cx);
    let published = ui
        .client
        .as_ref()
        .is_some_and(|c| c.state().borrow().voice.ptt_held);
    if !held && !published {
        return;
    }
    ui_mut(window, cx).ptt.release();
    if let Some(v) = voice(window, cx) {
        v.set_ptt_held(false);
    }
}

/// The key context every text field carries (gpui-component's inputs, the composer's included): push to talk never
/// acts while one holds the focus (ptt.ts `isEditable`).
const TEXT_FIELD_CONTEXT: &str = "Input";

/// AltGr is held (ptt.ts `getModifierState('AltGraph')`). On Linux xkb's Level3 is none of gpui's modifiers, so the
/// vendored platform keeps its state on the side (core/vendor/README.md, "gpui-pre-linux 0.3.12"); elsewhere AltGr comes
/// as Control+Alt, which the modifiers already refuse.
fn altgr_held() -> bool {
    #[cfg(target_os = "linux")]
    {
        gpui_linux::level3_shift_active()
    }
    #[cfg(not(target_os = "linux"))]
    {
        false
    }
}

/// The push-to-talk key's handlers, on the window's root element (review I1): an ancestor of whatever holds the focus,
/// so a key is heard on every screen. The key counts while this window's call is on.
pub fn ptt_keys<E: InteractiveElement>(w: WindowId, root: E) -> E {
    let key_of = move |cx: &App| {
        ui_of(w, cx)
            .filter(|u| in_a_call(u))
            .and_then(|u| u.ptt_key.clone())
    };
    root.capture_key_down(move |e: &gpui::KeyDownEvent, window, cx| {
        let stack = window.context_stack();
        // ptt.ts `inDialog`: a modal dialog (or the settings frame, its kin) lets go and takes no key.
        if stack
            .iter()
            .any(|c| c.contains(dilla_desktop_ui::components::dialog::OVERLAY_CONTEXT))
        {
            release_ptt(w, cx);
            return;
        }
        let Some((key, _)) = key_of(cx) else { return };
        // ptt.ts: only the bare key (no Ctrl, Alt, Shift, Cmd/Meta or AltGr, which comes as Ctrl+Alt), and never while
        // a text field holds the focus, where the key types as usual (Shift+Insert pastes).
        let m = e.keystroke.modifiers;
        if !e.keystroke.key.eq_ignore_ascii_case(&key)
            || m.control
            || m.alt
            || m.shift
            || m.platform
            || m.function
            || altgr_held()
            || stack.iter().any(|c| c.contains(TEXT_FIELD_CONTEXT))
        {
            return;
        }
        // ptt.ts `preventDefault`: the key is push to talk's, nobody else's.
        cx.stop_propagation();
        if e.is_held || ui_of(w, cx).is_some_and(|u| u.ptt.is_down()) {
            return;
        }
        ui_mut(w, cx).ptt.press();
        if let Some(v) = voice(w, cx) {
            v.set_ptt_held(true);
        }
    })
    .capture_key_up(move |e: &gpui::KeyUpEvent, _, cx| {
        let Some((key, release_ms)) = key_of(cx) else {
            return;
        };
        if !e.keystroke.key.eq_ignore_ascii_case(&key)
            || !ui_of(w, cx).is_some_and(|u| u.ptt.is_down())
        {
            return;
        }
        cx.stop_propagation();
        ui_mut(w, cx).ptt.let_go(ptt_now_ms());
        cx.spawn(async move |cx| {
            cx.background_executor()
                .timer(Duration::from_millis(u64::from(release_ms)))
                .await;
            cx.update(|cx| {
                let sending = ui_of(w, cx).is_some_and(|u| u.ptt.sending(ptt_now_ms(), release_ms));
                if !sending && let Some(v) = voice(w, cx) {
                    v.set_ptt_held(false);
                }
            });
        })
        .detach();
    })
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
    if let Some(page) = afk_stage(channel, w, cx) {
        return Some(page);
    }
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
    let person_menu = menu::person_menu(w, channel, cx);
    let profile_card = menu::profile_card(w, channel, window, cx);
    let values = client_of(w, cx).map(|c| c.voice_prefs().values());
    // A tile shown full screen whose stream is gone (or a stage that is not the call's) goes back, and the window with it.
    if let Some(k) = fullscreen_tile(w, cx) {
        let here = ui_of(w, cx).is_some_and(|u| {
            u.state.voice.call.channel == Some(channel.id)
                && tiles(&u.state, channel.id).contains(&k)
        });
        if !here {
            exit_tile_fullscreen(w, cx);
        }
    }
    let ui = ui_of(w, cx)?;
    let bounds = ui.menu.bounds.clone();
    let state = ui.state.clone();
    let focus = ui.stage_focus.clone()?;
    let keys = tiles(&state, channel.id);
    let focused = ui.focused.clone().filter(|k| keys.contains(k));
    let selected = ui.selected.min(keys.len().saturating_sub(1));
    let joined = joined(&state, channel.id);
    // VoiceView.tsx `Stage`: a channel this device's call is not in (nor joining) shows the preview, no tiles and no bar.
    let in_call =
        state.voice.call.phase != CallPhase::Idle && state.voice.call.channel == Some(channel.id);
    let stage_focused = focus.is_focused(window);
    let tile_views: Vec<AnyElement> = keys
        .iter()
        .enumerate()
        .map(|(i, k)| {
            tile(
                &state,
                channel,
                k,
                (stage_focused && i == selected, focused.as_ref(), false),
                (&images, &stats),
                (values.as_ref(), &bounds),
            )
        })
        .collect();
    // The tile shown full screen fills the window over everything else (CallTile.tsx `requestFullscreen` on the tile);
    // Escape gives the stage back.
    let full_view = ui.fullscreen.as_ref().map(|f| {
        let k = f.key.clone();
        let view = tile(
            &state,
            channel,
            &k,
            (false, Some(&k), true),
            (&images, &stats),
            (values.as_ref(), &bounds),
        );
        let size = window.viewport_size();
        deferred(
            anchored().position(gpui::point(px(0.0), px(0.0))).child(
                div()
                    .id("voice-tile-fullscreen-view")
                    .a11y(Role::Region, name_of(&state, Some(channel), k.user))
                    .track_focus(&f.focus)
                    .w(size.width)
                    .h(size.height)
                    .bg(gpui::black())
                    .occlude()
                    .on_key_down(|e: &gpui::KeyDownEvent, window, cx| {
                        if e.keystroke.key == "escape" {
                            cx.stop_propagation();
                            exit_tile_fullscreen(wid(window), cx);
                        }
                    })
                    .child(view),
            ),
        )
        .with_priority(6)
        .into_any_element()
    });
    let body = if !in_call {
        preview(&state, channel)
    } else if keys.is_empty() {
        div()
            .flex_1()
            .flex()
            .items_center()
            .justify_center()
            .child(line(
                t("voice.empty"),
                text::UI,
                line_height(text::UI, 1.3),
                color::FG_2,
            ))
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
    // Ruling Z: the voice header's topic is "{n} in voice" once joined, else "voice channel". Shell.tsx counts the
    // call's participants (devices), so a person on two devices counts twice there.
    let topic = if joined {
        let devices = state.voice.participants(channel.id).len();
        tf("voice.count", &[("n", devices.to_string().as_str())])
    } else {
        t("voice.header.idle").to_owned()
    };
    let title = stage_title(channel, IconName::Volume2, topic);
    let n = keys.len();
    let wide = f32::from(window.viewport_size().width) >= m::BAR_WORDS_MIN_W;
    let stage = div()
        .id("voice-stage")
        .landmark(Role::Region, channel.name.clone())
        .relative()
        .track_focus(&focus)
        .size_full()
        .flex()
        .flex_col()
        .bg(color::BG)
        .on_key_down(move |e, window, cx| {
            if dilla_desktop_ui::components::action_menu::opens_context_menu(&e.keystroke) {
                // Shift+F10 or the Menu key: the selected tile's person menu.
                let w = wid(window);
                let key = ui_of(w, cx).and_then(|u| {
                    let ch = u.state.voice.call.channel?;
                    let keys = tiles(&u.state, ch);
                    keys.get(u.selected.min(keys.len().saturating_sub(1)))
                        .cloned()
                });
                if let Some(key) = key {
                    cx.stop_propagation();
                    menu::open_from_key(w, key, cx);
                }
            } else {
                stage_key(&e.keystroke.key, n, wid(window), cx);
            }
            window.refresh();
        })
        .child(title)
        .children(ring_notice(&state, channel))
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
        .when(in_call, |d| d.child(call_bar(&state, channel, wide)))
        .children(menu)
        .children(person_menu)
        .children(profile_card)
        .children(full_view);
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
        // No media session: only a test's frames ([`VoiceUi::put_test_frame`]).
        #[cfg(feature = "test-hooks")]
        return ui_of(w, cx)
            .map(|u| u.test_frames.clone())
            .unwrap_or_default();
        #[cfg(not(feature = "test-hooks"))]
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

type TileImages = HashMap<StreamKey, Arc<RenderImage>>;
type TileStats = HashMap<StreamKey, (u32, u32)>;
type TileBounds = Rc<std::cell::RefCell<HashMap<TileKey, gpui::Bounds<gpui::Pixels>>>>;

fn tile(
    state: &AppState,
    channel: &Channel,
    k: &TileKey,
    (selected, focused, full): (bool, Option<&TileKey>, bool),
    (images, stats): (&TileImages, &TileStats),
    (values, bounds): (
        Option<&dilla_client::features::voice_prefs::values::VoiceValues>,
        &TileBounds,
    ),
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
    let has_media = video.is_some();
    let own = p.is_some_and(|p| p.own);
    // VoiceView.tsx `withTileStats`: a screen says its frames; the own person tile what its microphone sends (this
    // client's capture is always Opus at 48 kHz: `dilla_media`), once the call is up.
    let stat = video
        .is_some()
        .then(|| stats.get(&stream))
        .flatten()
        .map(|(h, fps)| frame_stat(*h, *fps))
        .or_else(|| {
            (own && !k.screen && state.voice.call.phase == CallPhase::In).then(|| {
                tf(
                    "callStage.stat.codec",
                    &[("codec", OWN_CODEC.0), ("khz", OWN_CODEC.1)],
                )
            })
        });
    // Signal §5.6: the own tile is "{name} (you)".
    let title = if own && !k.screen {
        tf("callStage.tile.you", &[("name", &name)])
    } else {
        name.clone()
    };
    let muted = p.is_some_and(|p| p.muted() || p.deafened());
    let deafened = p.is_some_and(VoiceParticipant::deafened);
    // CallTile.tsx: another person's tile says their volume while it is not 100 (the own tile and a screen have none).
    let volume = values
        .filter(|_| !own && !k.screen)
        .map(|v| menu::volume_of(v, k.user, false))
        .filter(|v| *v != 100);
    let is_focused = focused == Some(k);
    let face = if focused.is_some() && !is_focused {
        AvatarSize::Confirm
    } else {
        AvatarSize::Stage
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
    // The web draws the tile's edge as an inset shadow, which moves nothing; here it is a border, inside which absolute
    // children are placed, so their insets are counted from the tile's outer edge.
    let edge = if speaking || selected {
        m::TILE_EDGE_STRONG
    } else {
        m::TILE_EDGE
    };
    let caption = div()
        .absolute()
        .left(px(m::CAPTION_INSET - edge))
        .bottom(px(m::CAPTION_INSET - edge))
        .h(px(m::CAPTION_H))
        .overflow_hidden()
        .flex()
        .items_center()
        .gap(px(m::CAPTION_GAP))
        .px(px(m::CAPTION_CHIP_PAD_X))
        .rounded(radius::ROW)
        .bg(color::SCRIM_CAPTION)
        // CallTile.tsx's stage caption: the mute glyph, the name, then the three-bar live meter (drawn while speaking; it
        // holds its room otherwise).
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
        })
        .child(
            line(title, text::XS, line_height(text::XS, LINE), color::FG)
                .weight(font::WEIGHT_LABEL),
        )
        .child(
            div()
                .flex_none()
                .flex()
                .items_end()
                .gap(px(m::METER_GAP))
                .h(px(m::METER_H))
                .when(!speaking, |d| d.invisible())
                .children((0..3).map(|_| div().w(px(m::METER_W)).h(px(m::METER_H)).bg(color::OK))),
        )
        .children(volume.map(menu::volume_chip));
    let key = k.clone();
    let menu_key = k.clone();
    let bounds_key = k.clone();
    let bounds = bounds.clone();
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
        .border(px(edge))
        .border_color(if speaking {
            color::OK
        } else if selected {
            color::ACCENT
        } else {
            color::HAIRLINE
        })
        .cursor_pointer()
        .on_mouse_down(gpui::MouseButton::Right, move |e, window, cx| {
            menu::open(wid(window), menu_key.clone(), e.position, cx);
            window.refresh();
        })
        .child(
            gpui::canvas(
                move |b, _, _| {
                    bounds.borrow_mut().insert(bounds_key.clone(), b);
                },
                |_, (), _, _| {},
            )
            .absolute()
            .top_0()
            .left_0()
            .size_full(),
        )
        .on_click(move |e, window, cx| {
            let w = wid(window);
            // CallTile.tsx: a double-click on a tile with media goes full screen (the second click of it focuses
            // nothing more).
            if e.click_count() == 2 && has_media {
                toggle_tile_fullscreen(key.clone(), window, cx);
                return;
            }
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
            // `.d-call-stage__tile[data-focused] .d-call-tile__stat`: the focused tile's stat sits 10px over its bottom
            // right in 10px, under the tools.
            let at = div().absolute();
            let at = if is_focused {
                at.bottom(px(m::TOOLS_INSET - edge))
                    .right(px(m::TOOLS_INSET - edge))
            } else {
                at.top(px(m::STAT_TOP - edge))
                    .right(px(m::STAT_RIGHT - edge))
            };
            let size = if is_focused { text::MICRO } else { text::CHIP };
            d.child(at.child(line(stat, size, line_height(size, LINE), color::FG_3)))
        })
        .when(is_focused, |d| d.child(tile_tools(edge, k.clone(), full)))
        .into_any_element()
}

/// The focused tile's tools over its top right (CallTile.tsx `__tools`, 10px in, 4px apart): Grid (back to the grid; 28px
/// high, 9px in, the 13px glyph 6px before its word) and full screen (28 x 28, a 14px glyph, `--accent` while on: the
/// tile alone fills the window).
fn tile_tools(edge: f32, key: TileKey, full: bool) -> AnyElement {
    let tool = |id: &'static str| {
        div()
            .id(id)
            .flex()
            .items_center()
            .justify_center()
            .h(px(m::TOOL))
            .min_w(px(m::TOOL))
            .rounded(radius::MD)
            .bg(color::SCRIM_TILE)
            .cursor_pointer()
            .focus_visible(|s| s.shadow(focus_ring()))
    };
    div()
        .absolute()
        .top(px(m::TOOLS_INSET - edge))
        .right(px(m::TOOLS_INSET - edge))
        .flex()
        .gap(px(m::TOOLS_GAP))
        .child(
            tool("voice-tile-grid")
                .a11y(Role::Button, t("callStage.grid"))
                .tab_index(0)
                .debug_selector(|| "voice-tile-grid".into())
                .px(px(m::GRID_TOOL_PAD_X))
                .gap(px(m::GRID_TOOL_GAP))
                .tooltip(dilla_desktop_ui::components::tooltip::tooltip(
                    t("callStage.grid"),
                    &[],
                ))
                .on_click(|_, window, cx| {
                    cx.stop_propagation();
                    let w = wid(window);
                    exit_tile_fullscreen(w, cx);
                    if ui_of(w, cx).is_some() {
                        ui_mut(w, cx).focused = None;
                    }
                    window.refresh();
                })
                .child(
                    Icon::new(IconName::LayoutGrid)
                        .size(px(m::GRID_GLYPH))
                        .color(color::FG_STRONG),
                )
                .child(line(
                    t("callStage.grid"),
                    text::XS,
                    line_height(text::XS, LINE),
                    color::FG_STRONG,
                )),
        )
        .child(
            tool("voice-tile-fullscreen")
                .a11y(Role::Button, t("stage.fullscreen"))
                .toggled(full)
                .tab_index(0)
                .debug_selector(|| "voice-tile-fullscreen".into())
                .tooltip(dilla_desktop_ui::components::tooltip::tooltip(
                    t("stage.fullscreen"),
                    &[],
                ))
                .on_click(move |_, window, cx| {
                    cx.stop_propagation();
                    toggle_tile_fullscreen(key.clone(), window, cx);
                })
                .child(
                    Icon::new(if full {
                        IconName::Minimize
                    } else {
                        IconName::Maximize
                    })
                    .size(px(m::FULLSCREEN_GLYPH))
                    .color(if full {
                        color::ACCENT
                    } else {
                        color::FG_STRONG
                    }),
                ),
        )
        .into_any_element()
}

// ---- the DM call box ----------------------------------------------------------------------------------------------

/// The name a person shows in a DM call (VoiceView.tsx `userName`): their display name in any server this device
/// knows, else the account's own name, else the short id.
fn dm_name_of(state: &AppState, user: UserId) -> String {
    state
        .members
        .values()
        .flatten()
        .find(|m| m.user_id == user)
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

/// The DM call box's tiles (VoiceView.tsx `callTiles`): each device's person tile and, while it shares, its screen tile,
/// the shared screens first, then the people in the roster's order.
pub fn dm_tiles(state: &AppState, channel: ChannelId) -> Vec<TileKey> {
    let mut out = tiles(state, channel);
    out.sort_by_key(|k| !k.screen);
    out
}

/// The grid's columns (`repeat(auto-fill, minmax(min(100%, 11rem), 1fr))` with the stage's 10px gap) across `width`.
pub fn dm_columns(width: f32) -> usize {
    (((width + m::TILE_GAP) / (m::DM_TILE_MIN + m::TILE_GAP)).floor() as usize).max(1)
}

/// The outgoing ring's line over the tiles (VoiceView.tsx `DmCall`), while this device's DM call rings someone. Someone
/// else is in the call when a participant is another person (`userId !== self`).
fn outgoing_text(state: &AppState, channel: ChannelId) -> Option<String> {
    let out = state.ring.outgoing.as_ref()?;
    if out.channel != Some(channel) {
        return None;
    }
    let me = state.account.user_id;
    let own = state.account.device_id;
    let call = &state.voice.call;
    // `|| call.state.tiles.length > 0`: a tile is another device of the call group (the session's roster), which the
    // instance's participant read may not list yet.
    let others = state
        .voice
        .participants(channel)
        .iter()
        .any(|p| Some(p.user_id) != me)
        || (call.channel == Some(channel) && call.roster.iter().any(|r| Some(r.1) != own));
    outgoing_line(out, others, &|u| dm_name_of(state, u))
}

/// useCall.tsx `outgoingText`: `ringing {names}` while it rings and the ring has not run its 30 s, `{names} not rung`
/// for the participants the instance did not ring (with nobody else in the call), `{name} declined.` per decline, and
/// `No answer.` once every ring ended (30 s, or nobody left ringing or waiting) with nobody else in the call and nobody
/// declined; `None` when there is nothing to say.
pub fn outgoing_line(
    out: &dilla_client::state::ring::Outgoing,
    others: bool,
    name: &dyn Fn(UserId) -> String,
) -> Option<String> {
    let names = |users: &[UserId]| {
        users
            .iter()
            .map(|u| name(*u))
            .collect::<Vec<_>>()
            .join(", ")
    };
    let mut parts = Vec::new();
    if !out.ringing.is_empty() && !out.expired {
        parts.push(tf(
            "call.outgoing.ringing",
            &[("names", names(&out.ringing).as_str())],
        ));
    }
    if !out.not_rung.is_empty() && !others {
        parts.push(tf(
            "call.outgoing.notRung",
            &[("names", names(&out.not_rung).as_str())],
        ));
    }
    for u in &out.declined {
        parts.push(tf("call.outgoing.declined", &[("name", name(*u).as_str())]));
    }
    // useCall.tsx `unanswered`.
    let unanswered =
        !others && (out.expired || (out.ringing.is_empty() && out.not_rung.is_empty()));
    if unanswered && out.declined.is_empty() {
        parts.push(t("call.outgoing.noAnswer").to_owned());
    }
    (!parts.is_empty()).then(|| parts.join(" "))
}

/// The DM call (VoiceView.tsx `DmCall`, flow 06): while this device is in the DM's call, its box over the log, at most
/// 40 % of the conversation. It is the compact grid, never the channel stage: no focus layout, no film strip and no full
/// screen, so a shared screen shows at a card's size (e2e/voice/stage.spec.ts, WS7 review critical 1). Its head names
/// the DM, says the outgoing ring and holds the call code; the icon toolbar stays under the tiles while they scroll.
/// `None` while this device is not in the DM's call.
pub fn dm_call_box(channel: &Channel, window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let w = wid(window);
    let state = ui_of(w, cx)?.state.clone();
    if !joined(&state, channel.id) {
        // CallStage.tsx's `<details>` goes with the box: the next call's code starts closed.
        if ui_of(w, cx).is_some_and(|u| u.code_open) {
            ui_mut(w, cx).code_open = false;
        }
        return None;
    }
    let images = refresh_images(window, cx);
    // Before the grid's first layout its width is not measured yet: the window's width less the rail, the default
    // sidebar and the stage's padding stands in, so the first frame does not draw one tall column (review minor).
    let grid_w = match ui_of(w, cx).map_or(0.0, |u| u.dm_grid_w) {
        measured if measured > 0.0 => measured,
        _ => {
            f32::from(window.viewport_size().width - layout::RAIL - layout::SIDEBAR)
                - 2.0 * m::STAGE_PAD
        }
    };
    let keys = dm_tiles(&state, channel.id);
    let cards: Vec<AnyElement> = keys.iter().map(|k| card(&state, k, &images)).collect();
    let measure = gpui::canvas(
        move |bounds, window, cx| {
            let width = f32::from(bounds.size.width);
            if let Some(ui) = cx.global_mut::<VoiceAreas>().windows.get_mut(&w)
                && (ui.dm_grid_w - width).abs() > 0.5
            {
                ui.dm_grid_w = width;
                window.request_animation_frame();
            }
        },
        |_, (), _, _| {},
    )
    .absolute()
    .top_0()
    .left_0()
    .size_full();
    let grid = div()
        .w_full()
        .grid()
        .grid_cols(u16::try_from(dm_columns(grid_w)).unwrap_or(1))
        .gap(px(m::TILE_GAP))
        .children(cards);
    let code = state
        .voice
        .call
        .code
        .clone()
        .map(|code| code_disclosure(w, &code, cx));
    // `.d-call-stage__head` aligns its parts on their first baselines (`align-items: baseline`): each part stands as far
    // below the head's top as its baseline is above the lowest one. The call code's baseline is its summary's text,
    // centred in the 28px summary.
    let name_lh = line_height(text::MD, 1.3);
    let ring_lh = line_height(text::UI, 1.3);
    let code_lh = line_height(text::META, 1.3);
    let name_base = chromium_baseline(Face::Mono, f32::from(text::MD), name_lh);
    let ring_base = chromium_baseline(Face::Mono, f32::from(text::UI), ring_lh);
    let code_base =
        (m::CODE_H - code_lh) / 2.0 + chromium_baseline(Face::Mono, f32::from(text::META), code_lh);
    let ring = outgoing_text(&state, channel.id);
    let lowest = [
        Some(name_base),
        ring.as_ref().map(|_| ring_base),
        code.as_ref().map(|_| code_base),
    ]
    .into_iter()
    .flatten()
    .fold(0.0_f32, f32::max);
    let head = div()
        .flex()
        .flex_wrap()
        .items_start()
        .gap_x(px(m::HEAD_GAP_X))
        .gap_y(px(m::HEAD_GAP_Y))
        .min_w_0()
        .px(px(m::STAGE_PAD))
        .pt(px(m::STAGE_PAD))
        .child(div().mt(px(lowest - name_base)).child(
            line(channel.name.clone(), text::MD, name_lh, color::FG).weight(font::WEIGHT_LABEL),
        ))
        .children(ring.map(|s| {
            div()
                .mt(px(lowest - ring_base))
                .child(line(s, text::UI, ring_lh, color::FG_2))
        }))
        .children(code.map(|c| div().ml_auto().mt(px(lowest - code_base)).child(c)));
    let body = div()
        .p(px(m::STAGE_PAD))
        .child(div().relative().w_full().child(grid).child(measure));
    let scroll = div()
        .id("dm-call-tiles")
        .flex_shrink(1.0)
        .min_h_0()
        .overflow_y_scroll()
        .child(head)
        .children(notice_or_none(&state))
        .child(body);
    Some(
        div()
            .id("dm-call-box")
            .debug_selector(|| "dm-call-box".to_owned())
            .flex_none()
            .max_h(relative(m::DM_BOX_SHARE))
            .min_h_0()
            .flex()
            .flex_col()
            .bg(color::BG)
            .border_b_1()
            .border_color(color::HAIRLINE)
            .child(scroll)
            .child(dm_controls(&state))
            .into_any_element(),
    )
}

/// CallStage.tsx `groupCode`: six groups of five digits; anything that is not 30 digits is shown as given.
pub fn group_code(code: &str) -> String {
    let digits: String = code.chars().filter(|c| !c.is_whitespace()).collect();
    if digits.len() == 30 && digits.bytes().all(|b| b.is_ascii_digit()) {
        digits
            .as_bytes()
            .chunks(5)
            .map(|c| std::str::from_utf8(c).unwrap_or_default())
            .collect::<Vec<_>>()
            .join(" ")
    } else {
        code.to_owned()
    }
}

/// The DM box's call code (CallStage.tsx's `<details>`): its summary toggles on a click, Enter or Space; opened, its body
/// under the summary holds the grouped safety-code digits and the help. Escape closes it and keeps the focus on the
/// summary.
fn code_disclosure(w: WindowId, code: &str, cx: &mut App) -> AnyElement {
    let focus = {
        let have = ui_mut(w, cx).code_focus.clone();
        match have {
            Some(h) => h,
            None => {
                let h = cx.focus_handle();
                ui_mut(w, cx).code_focus = Some(h.clone());
                h
            }
        }
    };
    let open = ui_of(w, cx).is_some_and(|u| u.code_open);
    let summary = div()
        .id("dm-call-code")
        .debug_selector(move || {
            if open {
                "dm-call-code-open".to_owned()
            } else {
                "dm-call-code".to_owned()
            }
        })
        .a11y(Role::Button, t("voice.code"))
        .aria_expanded(open)
        .track_focus(&focus.clone().tab_stop(true))
        .h(px(m::CODE_H))
        .px(px(m::CODE_PAD_X))
        .flex()
        .items_center()
        .gap(px(m::CODE_GLYPH_GAP))
        .rounded(radius::MD)
        .cursor_pointer()
        .focus_visible(|s| s.shadow(focus_ring()))
        .child(
            Icon::new(if open {
                IconName::ChevronDown
            } else {
                IconName::ChevronRight
            })
            .size(px(f32::from(text::META) * m::CODE_GLYPH_EM))
            .color(color::FG_2),
        )
        .child(line(
            t("voice.code"),
            text::META,
            line_height(text::META, 1.3),
            color::FG_2,
        ))
        .on_click(move |_, _, cx| {
            let ui = ui_mut(w, cx);
            ui.code_open = !ui.code_open;
            cx.refresh_windows();
        });
    let body =
        open.then(|| {
            div()
                .debug_selector(|| "dm-call-code-digits".to_owned())
                .w_full()
                .max_w(px(m::CODE_BODY_MAX_W))
                .mt(px(m::CODE_BODY_GAP))
                .py(px(m::CODE_BODY_PAD_Y))
                .px(px(m::CODE_BODY_PAD_X))
                .bg(color::SURFACE_2)
                .border_1()
                .border_color(color::LINE_STRONG)
                .rounded(radius::MD)
                .child(
                    line(
                        group_code(code),
                        text::MD,
                        line_height(text::MD, 1.3),
                        color::FG,
                    )
                    .tracking(dilla_desktop_ui::metrics::LABEL_TRACKING),
                )
                .child(div().mt(px(m::CODE_HELP_GAP)).child(
                    dilla_desktop_ui::text::flow_paragraph(
                        t("voice.codeHelp"),
                        text::XS,
                        line_height(text::XS, 1.3),
                        color::FG_2,
                    ),
                ))
        });
    div()
        .ml_auto()
        .flex()
        .flex_col()
        .items_end()
        .on_key_down(move |e: &gpui::KeyDownEvent, window, cx| {
            if e.keystroke.key == "escape" && ui_of(w, cx).is_some_and(|u| u.code_open) {
                ui_mut(w, cx).code_open = false;
                focus.focus(window, cx);
                cx.stop_propagation();
                cx.refresh_windows();
            }
        })
        .child(summary)
        .children(body)
        .into_any_element()
}

/// The media notice over the tiles, or nothing while there is none.
fn notice_or_none(state: &AppState) -> Option<AnyElement> {
    state.voice.notice.is_some().then(|| notice(state))
}

/// One compact card of the DM call box (CallTile.tsx without `onFocusTile`): the media when a frame has come (16:9 at
/// the card's width), then the caption: the name and its glyph-only states. A person with no camera is the caption alone.
fn card(
    state: &AppState,
    k: &TileKey,
    images: &HashMap<StreamKey, Arc<RenderImage>>,
) -> AnyElement {
    let channel = state.voice.call.channel;
    let p = channel.and_then(|c| {
        state
            .voice
            .participants(c)
            .iter()
            .find(|p| p.device_id == k.device)
    });
    let own = p.is_some_and(|p| p.own);
    let name = dm_name_of(state, k.user);
    let title = if k.screen {
        tf("stage.screen", &[("name", name.as_str())])
    } else if own {
        tf("callStage.tile.you", &[("name", name.as_str())])
    } else {
        name
    };
    let stream = StreamKey {
        device: k.device.hex(),
        kind: if k.screen {
            ShareKind::Screen
        } else {
            ShareKind::Camera
        },
    };
    let video = images.get(&stream).cloned();
    let (muted, deafened) = if own {
        (state.voice.self_mute, state.voice.self_deaf)
    } else {
        (
            p.is_some_and(VoiceParticipant::muted),
            p.is_some_and(VoiceParticipant::deafened),
        )
    };
    // A screen's caption carries no person states (only a share's no-sound mark, which comes with capture).
    let glyphs: Vec<(IconName, gpui::Hsla)> = if k.screen {
        Vec::new()
    } else {
        [
            (muted, IconName::MicOff, color::DANGER),
            (deafened, IconName::HeadphoneOff, color::DANGER),
            (
                p.is_some_and(VoiceParticipant::video),
                IconName::Video,
                color::FG_2,
            ),
            (
                p.is_some_and(VoiceParticipant::screen),
                IconName::MonitorUp,
                color::FG_2,
            ),
        ]
        .into_iter()
        .filter_map(|(on, icon, tone)| on.then_some((icon, tone)))
        .collect()
    };
    let speaking = p.is_some_and(|p| state.voice.speaking.contains(&p.device_id));
    let edge = if speaking {
        m::TILE_EDGE_STRONG
    } else {
        m::TILE_EDGE
    };
    let caption = div()
        .flex()
        .items_center()
        .justify_center()
        .gap(px(m::CAPTION_GAP))
        .min_h(px(m::CAPTION_MIN_H))
        .max_w_full()
        .min_w_0()
        .child(
            div().min_w_0().overflow_hidden().child(
                line(title, text::XS, line_height(text::XS, 1.3), color::FG)
                    .weight(font::WEIGHT_LABEL)
                    .ellipsis(),
            ),
        )
        .children(
            glyphs
                .into_iter()
                .map(|(icon, tone)| Icon::new(icon).size(px(m::GLYPH)).color(tone)),
        );
    let id = format!(
        "dm-tile-{}{}",
        k.device.hex(),
        if k.screen { "-screen" } else { "" }
    );
    let selector = id.clone();
    let media_selector = format!("{id}-media");
    div()
        .id(ElementId::Name(id.into()))
        .debug_selector(move || selector.clone())
        .min_w_0()
        .flex()
        .flex_col()
        .items_center()
        .justify_center()
        .gap(px(m::CARD_GAP))
        // The web's edge is an inset shadow, which takes no room: the border here stands inside the padding.
        .p(px(m::DM_TILE_PAD - edge))
        .bg(color::STAGE_TILE)
        .rounded(radius::XL)
        .border(px(edge))
        .border_color(if speaking { color::OK } else { color::HAIRLINE })
        .children(video.map(|image| {
            div()
                .debug_selector(move || media_selector.clone())
                .w_full()
                .aspect_ratio(16.0 / 9.0)
                .rounded(radius::MD)
                .overflow_hidden()
                .bg(color::BG)
                .child(
                    img(ImageSource::Render(image))
                        .size_full()
                        .object_fit(gpui::ObjectFit::Contain),
                )
        }))
        .child(caption)
        .into_any_element()
}

/// The DM call box's controls (CallControls without the call bar's words): mute, deafen, camera, share, then leave set
/// off by a hairline, 8px above and below.
fn dm_controls(state: &AppState) -> AnyElement {
    let v = &state.voice;
    let muted = v.self_mute || v.self_deaf;
    div()
        .flex_none()
        .flex()
        .justify_center()
        .px(px(m::STAGE_PAD))
        .py(px(m::DM_CONTROLS_PAD_Y))
        .bg(color::BG)
        .child(
            div()
                .flex()
                .flex_wrap()
                .items_stretch()
                .gap(px(m::TOOLBAR_GAP))
                .child(toolbar_control(
                    "dm-call-mute",
                    if muted {
                        IconName::MicOff
                    } else {
                        IconName::Mic
                    },
                    t("voice.mute"),
                    Some(muted),
                    false,
                    |_, window, cx| toggle_mute_in(wid(window), cx),
                ))
                .child(toolbar_control(
                    "dm-call-deafen",
                    if v.self_deaf {
                        IconName::HeadphoneOff
                    } else {
                        IconName::Headphones
                    },
                    t("voice.deafen"),
                    Some(v.self_deaf),
                    false,
                    |_, window, cx| toggle_deafen_in(wid(window), cx),
                ))
                .child(toolbar_control(
                    "dm-call-camera",
                    if v.share.camera {
                        IconName::Video
                    } else {
                        IconName::VideoOff
                    },
                    t("callStage.bar.camera"),
                    Some(v.share.camera),
                    false,
                    |_, window, cx| toggle_share(ShareKind::Camera, wid(window), cx),
                ))
                .child(toolbar_control(
                    "dm-call-share",
                    if v.share.screen {
                        IconName::MonitorX
                    } else {
                        IconName::MonitorUp
                    },
                    t("callStage.bar.share"),
                    Some(v.share.screen),
                    false,
                    |_, window, cx| toggle_share(ShareKind::Screen, wid(window), cx),
                ))
                .child(
                    div()
                        .flex_none()
                        .w_0()
                        .my(px(m::DIVIDER_INSET_Y))
                        .mx(px(m::DIVIDER_INSET_X))
                        .border_l_1()
                        .border_color(color::HAIRLINE_2),
                )
                .child(toolbar_control(
                    "dm-call-leave",
                    IconName::PhoneOff,
                    t("voice.leave"),
                    None,
                    true,
                    |_, window, cx| leave(wid(window), cx),
                )),
        )
        .into_any_element()
}

/// One control of the DM call box's toolbar (CallControls.css `.d-voice-toolbar__control`, a `d-btn`): 40px with the
/// 16px glyph, a `--control-edge` edge on nothing; leave the `danger` variant (`--danger` on its `--danger-border` edge).
/// A toggled-on control is `--danger` on `--surface-2` with its edge and the 2px underline.
fn toolbar_control(
    id: &'static str,
    glyph: IconName,
    label: &str,
    on: Option<bool>,
    danger: bool,
    on_click: impl Fn(&gpui::ClickEvent, &mut Window, &mut App) + 'static,
) -> impl IntoElement {
    let pressed = on == Some(true);
    let (fg, edge) = if pressed || danger {
        (
            color::DANGER,
            if pressed {
                color::DANGER
            } else {
                color::DANGER_BORDER
            },
        )
    } else {
        (color::FG, color::CONTROL_EDGE)
    };
    let button = div()
        .id(id)
        .a11y(Role::Button, label.to_owned())
        .toggled(pressed)
        .tab_index(0)
        .relative()
        .flex_none()
        .min_w(px(m::BAR_BUTTON_H))
        .min_h(px(m::BAR_BUTTON_H))
        .px(px(m::TOOLBAR_PAD_X))
        .flex()
        .items_center()
        .justify_center()
        .rounded(radius::MD)
        .border_1()
        .border_color(edge)
        .when(pressed, |d| d.bg(color::SURFACE_2))
        .cursor_pointer()
        .focus_visible(|s| s.shadow(focus_ring()))
        .tooltip(dilla_desktop_ui::components::tooltip::tooltip(
            label.to_owned(),
            &[],
        ))
        .child(Icon::new(glyph).size(px(m::BAR_GLYPH)).color(fg))
        .when(pressed, |d| {
            d.child(
                div()
                    .absolute()
                    .left_0()
                    .right_0()
                    .bottom_0()
                    .h(px(m::TOOLBAR_UNDERLINE))
                    .bg(fg),
            )
        })
        .on_click(on_click);
    tracked_box(id, button)
}

/// A stage notice's box: the warning glyph and its line (the media notice and the ring area's notices).
fn notice_box(text: String) -> Div {
    div()
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
                .child(line(text, text::UI, line_height(text::UI, 1.3), color::FG)),
        )
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
        .a11y(Role::Status, t(key))
        .live(dilla_desktop_ui::a11y::Live::Polite)
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

/// The not-joined preview (CallStage.tsx `preview`, Signal §5.6): the faces of who is inside (each 48px, 10px over the
/// one before with a 3px `--bg` ring, a 5px `--ok` ring on whoever speaks), the room (16px 700), who is there (12px
/// `--fg-2`) and the green Join voice. No join while a block holds it back (Flow 06, Task 18's `join_held`).
fn preview(state: &AppState, channel: &Channel) -> AnyElement {
    let people = people(state, channel);
    let names: Vec<&str> = people.iter().map(|p| p.name.as_str()).collect();
    let who = match names.as_slice() {
        [] => t("callStage.preview.nobody").to_owned(),
        [one] => tf("callStage.preview.one", &[("name", one)]),
        many => tf(
            "callStage.preview.many",
            &[("names", &many.join(t("callStage.preview.and")))],
        ),
    };
    let faces = div().flex().children(people.iter().map(|p| {
        let ring = div()
            .absolute()
            .top(px(-m::FACE_RING))
            .left(px(-m::FACE_RING))
            .size(px(m::FACE + 2.0 * m::FACE_RING))
            .rounded(radius::AVATAR)
            .bg(color::BG);
        let ring = if p.speaking {
            ring.border(px(m::FACE_SPEAKING - m::FACE_RING))
                .border_color(color::OK)
                .top(px(-m::FACE_SPEAKING))
                .left(px(-m::FACE_SPEAKING))
                .size(px(m::FACE + 2.0 * m::FACE_SPEAKING))
        } else {
            ring
        };
        div()
            .relative()
            .flex_none()
            .ml(px(-m::FACE_OVERLAP))
            .size(px(m::FACE))
            .child(ring)
            .child(
                Avatar::new(
                    ElementId::Name(format!("voice-preview-face-{}", p.user.hex()).into()),
                    p.name.clone(),
                )
                .size(AvatarSize::StagePreview),
            )
    }));
    let text_block = div()
        .flex()
        .flex_col()
        .items_center()
        .gap(px(m::PREVIEW_TEXT_GAP))
        .child(
            line(
                channel.name.clone(),
                text::PREVIEW,
                line_height(text::PREVIEW, LINE),
                color::FG,
            )
            .weight(font::WEIGHT_DISPLAY),
        )
        .child(line(
            who,
            text::UI,
            line_height(text::UI, LINE),
            color::FG_2,
        ));
    let id = channel.id;
    let join_button = (!crate::screens::ring::join_held(state, channel.id)).then(|| {
        tracked_box(
            "voice-join",
            div()
                .id("voice-join-b")
                .a11y(Role::Button, t("voice.join"))
                .tab_index(0)
                .h(px(m::JOIN_H))
                .px(px(m::JOIN_PAD_X))
                .flex()
                .items_center()
                .gap(px(m::JOIN_GAP))
                .rounded(radius::LG)
                .bg(color::OK)
                .cursor_pointer()
                .focus_visible(|s| s.shadow(focus_ring()))
                .child(
                    Icon::new(IconName::Phone)
                        .size(px(m::JOIN_GLYPH))
                        .color(color::OK_INK),
                )
                .child(
                    line(
                        t("callStage.preview.join"),
                        text::SM,
                        line_height(text::SM, LINE),
                        color::OK_INK,
                    )
                    .weight(font::WEIGHT_DISPLAY),
                )
                .on_click(move |_, window, cx| join(id, wid(window), cx)),
        )
    });
    div()
        .id("voice-preview")
        .flex_1()
        .min_h_0()
        .flex()
        .flex_col()
        .items_center()
        .justify_center()
        .gap(px(m::PREVIEW_GAP))
        .p(px(m::PREVIEW_PAD))
        .child(faces)
        .child(text_block)
        .children(join_button)
        .into_any_element()
}

/// The shortcut keycaps of the call bar's mute and deafen (shortcuts.ts `voiceKeycapText`).
fn keycaps() -> (&'static str, &'static str) {
    if cfg!(target_os = "macos") {
        ("⌘⇧M", "⌘⇧D")
    } else {
        ("Ctrl⇧M", "Ctrl⇧D")
    }
}

/// A keycap split where the web's JetBrains Mono subset lacks a glyph (`mono_covers` is the embedded full face, so the
/// ⇧ is listed): `(run, drawn in the symbol face)`.
fn keycap_runs(keycap: &str) -> Vec<(&str, bool)> {
    let mut out = Vec::new();
    let mut start = 0;
    for (i, c) in keycap.char_indices() {
        if c == '⇧' {
            if start < i {
                out.push((&keycap[start..i], false));
            }
            out.push((&keycap[i..i + c.len_utf8()], true));
            start = i + c.len_utf8();
        }
    }
    if start < keycap.len() {
        out.push((&keycap[start..], false));
    }
    out
}

/// How a call bar control is tinted (CallControls.css `.d-call-controls--bar`).
#[derive(Clone, Copy, PartialEq)]
enum Tint {
    /// Idle: `--surface-tile` with a `--hairline-2` edge.
    Idle,
    /// A toggled-on mute or deafen: the `--danger` tint.
    Danger,
    /// A live camera or share: `--accent` with `--accent-ink`.
    Accent,
    /// Leave: `--danger` with `--danger-ink`, 700.
    Leave,
}

/// One control of the call bar: the 16px glyph, its word and (mute, deafen) the shortcut's keycap. Below a 1240px window
/// the words and keycaps are not drawn (Leave keeps its word), as CallControls.css's `max-width: 77.49rem`.
fn bar_control(
    id: &'static str,
    glyph: IconName,
    word: &str,
    keycap: Option<&'static str>,
    tint: Tint,
    wide: bool,
    on_click: impl Fn(&gpui::ClickEvent, &mut Window, &mut App) + 'static,
) -> impl IntoElement {
    let (fg, bg, edge) = match tint {
        Tint::Idle => (color::FG, color::SURFACE_TILE, Some(color::HAIRLINE_2)),
        Tint::Danger => (
            color::DANGER,
            color::DANGER_SOFT,
            Some(color::DANGER_BORDER),
        ),
        Tint::Accent => (color::ACCENT_INK, color::ACCENT, Some(color::ACCENT)),
        Tint::Leave => (color::DANGER_INK, color::DANGER, None),
    };
    let leave = tint == Tint::Leave;
    let words = wide || leave;
    let button = div()
        .id(id)
        .a11y(Role::Button, word.to_owned())
        .tab_index(0)
        .flex_none()
        .h(px(m::BAR_BUTTON_H))
        .min_w(px(m::BAR_BUTTON_MIN_W))
        .px(px(if leave {
            m::BAR_LEAVE_PAD_X
        } else {
            m::BAR_BUTTON_PAD_X
        }))
        .flex()
        .items_center()
        .justify_center()
        .gap(px(m::BAR_BUTTON_GAP))
        .rounded(radius::SEG)
        .bg(bg)
        .cursor_pointer()
        .focus_visible(|s| s.shadow(focus_ring()))
        .when_some(edge, |d, edge| d.border_1().border_color(edge))
        .when(leave, |d| {
            d.ml(px(m::BAR_LEAVE_GAP))
                .hover(|s| s.bg(color::DANGER_HOVER))
        })
        .child(Icon::new(glyph).size(px(m::BAR_GLYPH)).color(fg))
        .when(words, |d| {
            d.child(
                line(word.to_owned(), text::BTN, line_height(text::BTN, LINE), fg).weight(
                    if leave {
                        font::WEIGHT_DISPLAY
                    } else {
                        font::WEIGHT_BTN
                    },
                ),
            )
        })
        .when_some(keycap.filter(|_| words), |d, keycap| {
            // The web's JetBrains Mono has no ⇧: Chromium draws it in DejaVu Sans (`Face::Symbol`), the rest in the mono.
            let ink = fg.opacity(m::KEYCAP_OPACITY);
            let lh = line_height(text::MICRO, LINE);
            d.child(div().flex().children(keycap_runs(keycap).into_iter().map(
                move |(run, symbol)| {
                    let l = line(run.to_owned(), text::MICRO, lh, ink);
                    if symbol { l.face(Face::Symbol) } else { l }
                },
            )))
        })
        .on_click(on_click);
    tracked_box(id, button)
}

/// The 64px call bar (CallStage.tsx `bar`, Signal §5.6): the room with its `--ok` dot and the stats line under it on the
/// left, then mute, deafen, camera, share (with the stream-quality chevron while sharing) and leave, worded. The stats
/// are the link's ping as the dock reads it ([`ping_text`]: `voice.dock.pingNone` until the media reads one).
fn call_bar(state: &AppState, channel: &Channel, wide: bool) -> AnyElement {
    let v = &state.voice;
    let muted = v.self_mute || v.self_deaf;
    let (mute_key, deafen_key) = keycaps();
    let where_ = div()
        .flex_1()
        .min_w_0()
        .mr_auto()
        .flex()
        .flex_col()
        .overflow_hidden()
        .child(
            div()
                .flex()
                .items_center()
                .gap(px(m::BAR_ROOM_GAP))
                .min_w_0()
                .child(
                    div()
                        .flex_none()
                        .size(px(m::BAR_DOT))
                        .rounded(radius::AVATAR)
                        .bg(color::OK),
                )
                .child(
                    line(
                        channel.name.clone(),
                        text::UI,
                        line_height(text::UI, BAR_LINE),
                        color::FG,
                    )
                    .weight(font::WEIGHT_LABEL)
                    .ellipsis(),
                ),
        )
        .child(
            line(
                link_text(v.call.ping_ms, v.call.rate_kbps),
                text::META,
                line_height(text::META, BAR_LINE),
                color::FG_3,
            )
            .ellipsis(),
        );
    let sharing = v.share.screen;
    let share = div()
        .flex()
        .items_center()
        .child(bar_control(
            "voice-share",
            IconName::MonitorUp,
            t(if sharing {
                "callStage.bar.stopShare"
            } else {
                "callStage.bar.share"
            }),
            None,
            if sharing { Tint::Accent } else { Tint::Idle },
            wide,
            move |_, window, cx| {
                // CallControls.tsx: before a share Share opens the picker; while sharing it stops the share.
                let w = wid(window);
                if sharing {
                    toggle_share(ShareKind::Screen, w, cx);
                } else if ui_of(w, cx).is_some() {
                    let ui = ui_mut(w, cx);
                    ui.share_menu = !ui.share_menu;
                    window.refresh();
                }
            },
        ))
        .when(sharing, |d| {
            d.child(tracked_box(
                "voice-share-quality",
                div()
                    .id("voice-share-quality-b")
                    .a11y(Role::Button, t("callStage.share.quality"))
                    .tab_index(0)
                    .w(px(m::QUALITY_W))
                    .h(px(m::BAR_BUTTON_H))
                    .ml(px(m::QUALITY_SEAM))
                    .flex()
                    .items_center()
                    .justify_center()
                    .bg(color::ACCENT)
                    .border_1()
                    .border_color(color::ACCENT)
                    .rounded_r(radius::SEG)
                    .relative()
                    // The left edge is the 0.25 ink seam (`border-left-color: var(--accent-split)`).
                    .child(
                        div()
                            .absolute()
                            .left_0()
                            .top_0()
                            .bottom_0()
                            .w(px(m::SEAM))
                            .bg(color::ACCENT_SPLIT),
                    )
                    .cursor_pointer()
                    .focus_visible(|s| s.shadow(focus_ring()))
                    .child(
                        Icon::new(IconName::ChevronUp)
                            .size(px(m::QUALITY_GLYPH))
                            .color(color::ACCENT_INK),
                    )
                    .on_click(|_, window, cx| {
                        let w = wid(window);
                        if ui_of(w, cx).is_some() {
                            let ui = ui_mut(w, cx);
                            ui.share_menu = !ui.share_menu;
                            window.refresh();
                        }
                    }),
            ))
        });
    div()
        .id("voice-call-bar")
        .landmark(Role::Region, t("voice.controls.region"))
        .flex_none()
        .min_h(px(m::CONTROLS_H))
        .flex()
        .items_center()
        .justify_end()
        .gap(px(m::CONTROLS_GAP))
        .px(px(m::BAR_PAD_X))
        .py(px(m::BAR_PAD_Y))
        .border_t_1()
        .border_color(color::HAIRLINE)
        .bg(color::BG_2)
        .child(where_)
        .child(bar_control(
            "voice-mute",
            if muted {
                IconName::MicOff
            } else {
                IconName::Mic
            },
            t(if muted {
                "callStage.bar.unmute"
            } else {
                "callStage.bar.mute"
            }),
            Some(mute_key),
            if muted { Tint::Danger } else { Tint::Idle },
            wide,
            |_, window, cx| toggle_mute_in(wid(window), cx),
        ))
        .child(bar_control(
            "voice-deafen",
            if v.self_deaf {
                IconName::HeadphoneOff
            } else {
                IconName::Headphones
            },
            t(if v.self_deaf {
                "callStage.bar.undeafen"
            } else {
                "callStage.bar.deafen"
            }),
            Some(deafen_key),
            if v.self_deaf {
                Tint::Danger
            } else {
                Tint::Idle
            },
            wide,
            |_, window, cx| toggle_deafen_in(wid(window), cx),
        ))
        .child(bar_control(
            "voice-camera",
            if v.share.camera {
                IconName::Video
            } else {
                IconName::VideoOff
            },
            t("callStage.bar.camera"),
            None,
            if v.share.camera {
                Tint::Accent
            } else {
                Tint::Idle
            },
            wide,
            |_, window, cx| toggle_share(ShareKind::Camera, wid(window), cx),
        ))
        .child(share)
        .child(bar_control(
            "voice-stage-leave",
            IconName::PhoneOff,
            t("callStage.bar.leave"),
            None,
            Tint::Leave,
            wide,
            |_, window, cx| leave(wid(window), cx),
        ))
        .into_any_element()
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
                line_height(text::UI, 1.3),
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
    /// Wave 2 review I-G1, useCall.tsx `outgoingText` (VoiceView.wb2.test.tsx's cases): the ring's line before and after
    /// its 30 s, with and without someone else in the call, declined and not rung.
    #[test]
    fn the_outgoing_line_follows_the_ring_clock() {
        use dilla_client::state::ring::Outgoing;
        let (bob, cy) = (UserId([2; 16]), UserId([3; 16]));
        let name = |u: UserId| {
            if u == bob {
                "bob".to_owned()
            } else {
                "cy".to_owned()
            }
        };
        let line = |out: &Outgoing, others: bool| outgoing_line(out, others, &name);
        let ringing = Outgoing {
            ringing: vec![bob],
            ..Outgoing::default()
        };
        assert_eq!(line(&ringing, false).as_deref(), Some("ringing bob"));
        // 30 s on with nobody answering: no longer ringing, "No answer.".
        let expired = Outgoing {
            expired: true,
            ..ringing.clone()
        };
        assert_eq!(line(&expired, false).as_deref(), Some("No answer."));
        // Someone else is in the call: nothing to say once expired.
        assert_eq!(line(&expired, true), None);
        // Not rung: said, and "No answer." waits for the ring time.
        let not_rung = Outgoing {
            not_rung: vec![bob],
            ..Outgoing::default()
        };
        let said = line(&not_rung, false).unwrap();
        assert!(!said.contains("No answer."), "{said}");
        assert!(!said.contains("ringing"), "{said}");
        let said = line(
            &Outgoing {
                expired: true,
                ..not_rung
            },
            false,
        )
        .unwrap();
        assert!(said.ends_with("No answer."), "{said}");
        // A decline is said, and no "No answer." after it, expired or not.
        let declined = Outgoing {
            declined: vec![cy],
            expired: true,
            ..Outgoing::default()
        };
        assert_eq!(line(&declined, false).as_deref(), Some("cy declined."));
    }

    use super::*;
    use dilla_client::state::voice::flags;

    /// VoiceView.tsx `DmCall`'s `others`: someone else is in the call when a participant is another person, or when the
    /// call has a tile of another device (`call.state.tiles.length > 0`: another member of the call group, the roster here,
    /// before the instance's participant read lists them). Then an expired ring says nothing, not "No answer.".
    #[test]
    fn another_device_in_the_call_group_is_someone_else_in_the_call() {
        use dilla_client::calls::CallPhase;
        use dilla_client::state::ring::Outgoing;
        use dilla_client::state::voice::OwnCall;
        let (me, bob) = (UserId([1; 16]), UserId([2; 16]));
        let (mine, bobs) = (DeviceId([0x11; 16]), DeviceId([0x22; 16]));
        let dm = ChannelId([0xd4; 16]);
        let mut s = AppState::default();
        s.account.user_id = Some(me);
        s.account.device_id = Some(mine);
        s.ring.outgoing = Some(Outgoing {
            channel: Some(dm),
            ringing: vec![bob],
            expired: true,
            ..Outgoing::default()
        });
        s.voice.call = OwnCall {
            phase: CallPhase::In,
            channel: Some(dm),
            roster: vec![(me, mine, 0)],
            ..OwnCall::default()
        };
        assert_eq!(outgoing_text(&s, dm).as_deref(), Some("No answer."));
        s.voice.call.roster.push((bob, bobs, 1));
        assert_eq!(outgoing_text(&s, dm), None, "bob's device is in the call");
    }

    /// VoiceView.tsx `Dock`'s ping: "— ms" until a ping is read, "<1 ms" under a millisecond, else "{ms} ms".
    #[test]
    fn the_ping_reads_as_the_web_s_dock_says_it() {
        assert_eq!(ping_text(None), "— ms");
        assert_eq!(ping_text(Some(0)), "<1 ms");
        assert_eq!(ping_text(Some(42)), "42 ms");
        // Package 8c: the rate follows the ping once one was read.
        assert_eq!(link_text(Some(24), None), "24 ms");
        assert_eq!(
            link_text(Some(24), Some((38, 71))),
            "24 ms · ↑ 38 ↓ 71 kb/s"
        );
    }

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

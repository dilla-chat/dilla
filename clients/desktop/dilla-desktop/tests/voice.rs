//! Task 17's tests for the voice area's views and keys (standard procedure S5-S6, the parts that run without a
//! network): the hooks draw from the voice slice, the stage's arrows, Enter and Escape move its selection and focus
//! mode, and the stage has a tile per stream. The parity tests (S4) and the two-device behaviour tests through
//! `Driver` wait on the shell's stage hook and the call seeding of the parity world (see the task report).

use std::sync::Arc;

use dilla_client::calls::CallPhase;
use dilla_client::state::voice::{OwnCall, VoiceParticipant, flags};
use dilla_client::state::{
    AppState, Channel, ChannelGroupState, ChannelId, CommunityId, DeviceId, Member, UserId,
};
use dilla_desktop::screens::voice::{self, VoiceUi};
use gpui::{
    App, Context, FocusHandle, IntoElement, ParentElement, Render, Styled, TestAppContext,
    VisualTestContext, Window, WindowId, div,
};

const VOICE: ChannelId = ChannelId([9; 16]);
const COMMUNITY: CommunityId = CommunityId([1; 16]);

fn channel() -> Channel {
    Channel {
        id: VOICE,
        community_id: COMMUNITY,
        kind: 1,
        mode: 0,
        name: "Lounge".into(),
        topic: String::new(),
        parent_id: None,
        position: 0,
        slowmode: 0,
        text_group_id: None,
        group: ChannelGroupState::Unsupported,
    }
}

/// A call of `n` people; the viewer (user 0) is in it; user 1 shares a screen and user 2 speaks.
fn state(n: u8) -> Arc<AppState> {
    let mut s = AppState::default();
    s.channels.push(channel());
    s.account.user_id = Some(UserId([0; 16]));
    s.account.device_id = Some(DeviceId([100; 16]));
    s.members.insert(
        COMMUNITY,
        (0..n)
            .map(|i| Member {
                user_id: UserId([i; 16]),
                username: format!("person{i}"),
                display: format!("A rather long display name number {i}"),
                kind: 0,
                role_ids: Vec::new(),
                status_msg: String::new(),
                bio: String::new(),
            })
            .collect(),
    );
    s.voice.place(
        VOICE,
        Some(VOICE.0),
        (0..n)
            .map(|i| VoiceParticipant {
                user_id: UserId([i; 16]),
                device_id: DeviceId([100 + i; 16]),
                flags: flags::IN_CALL | if i == 1 { flags::SCREEN } else { 0 },
                own: i == 0,
            })
            .collect(),
    );
    s.voice.speaking.insert(DeviceId([102; 16]));
    s.voice.call = OwnCall {
        phase: CallPhase::In,
        channel: Some(VOICE),
        community: Some(COMMUNITY),
        call_id: Some(VOICE.0),
        ..OwnCall::default()
    };
    Arc::new(s)
}

struct Host;

impl Render for Host {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let ch = channel();
        div()
            .size_full()
            .flex()
            .children(voice::channel_people(&ch, window, cx))
            .children(voice::dock(window, cx))
            .children(voice::stage(&ch, window, cx))
    }
}

fn host(cx: &mut TestAppContext, s: Arc<AppState>) -> &mut VisualTestContext {
    cx.update(dilla_desktop::install);
    let (_, cx) = cx.add_window_view(|_, _| Host);
    cx.update(|window, cx| {
        voice::sync(voice::wid(window), s, None, cx);
        window.refresh();
    });
    cx.run_until_parked();
    cx
}

/// The voice area of the test's window.
fn area<R>(cx: &mut VisualTestContext, f: impl FnOnce(&mut VoiceUi) -> R) -> R {
    cx.update(|window, cx| f(voice::ui_mut(voice::wid(window), cx)))
}

fn stage_focus(cx: &mut VisualTestContext) -> FocusHandle {
    area(cx, |ui| ui.stage_focus_handle().expect("the stage drew"))
}

fn press(cx: &mut VisualTestContext, key: &'static str) {
    cx.simulate_keystrokes(key);
    cx.run_until_parked();
}

/// e2e/voice/stage.spec.ts (the view half): a tile per person and one per shared screen; Enter focuses the selected
/// tile, the arrows move the selection, Escape leaves focus mode.
#[gpui::test]
fn stage_tile_per_stream_focuses_share(cx: &mut TestAppContext) {
    let cx = host(cx, state(3));
    let tiles = area(cx, |ui| voice::tiles(&ui.state, VOICE));
    assert_eq!(tiles.len(), 4, "three people and one screen");
    assert!(tiles[3].screen);
    let focus = stage_focus(cx);
    cx.update(|window, cx| focus.focus(window, cx));
    for _ in 0..3 {
        press(cx, "right");
    }
    assert_eq!(area(cx, |ui| ui.selected), 3);
    press(cx, "enter");
    let focused = area(cx, |ui| ui.focused.clone());
    assert_eq!(
        focused,
        Some(tiles[3].clone()),
        "the screen share is focused"
    );
    press(cx, "left");
    assert_eq!(area(cx, |ui| ui.selected), 2);
    press(cx, "escape");
    assert_eq!(area(cx, |ui| ui.focused.clone()), None);
}

/// Twelve people with long names (the -b fixtures' world) draw without a panic, and the people group per user.
#[gpui::test]
fn twelve_people_with_long_names_draw(cx: &mut TestAppContext) {
    let s = state(12);
    let people = voice::people(&s, &channel());
    assert_eq!(people.len(), 12);
    assert!(people[2].speaking && !people[3].speaking);
    assert!(people[1].screen);
    assert_eq!(voice::count(&s, VOICE), 12);
    let cx = host(cx, s);
    cx.run_until_parked();
}

/// The call ending clears the stage's focus and selection.
#[gpui::test]
fn leaving_resets_the_stage(cx: &mut TestAppContext) {
    let cx = host(cx, state(3));
    area(cx, |ui| {
        ui.selected = 2;
        ui.focused = Some(voice::tiles(&ui.state, VOICE)[0].clone());
    });
    let mut idle = (*state(3)).clone();
    idle.voice.call = OwnCall::default();
    cx.update(|window, cx| voice::sync(voice::wid(window), Arc::new(idle), None, cx));
    let (selected, focused) = area(cx, |ui| (ui.selected, ui.focused.clone()));
    assert_eq!((selected, focused), (0, None));
    assert!(!voice::joined(&area(cx, |ui| ui.state.clone()), VOICE));
}

/// The stream-quality menu opens over the controls with the share options (1080p at 30 by default) and draws.
#[gpui::test]
fn the_quality_menu_draws_the_share_options(cx: &mut TestAppContext) {
    let cx = host(cx, state(2));
    area(cx, |ui| {
        assert_eq!(ui.share_options, dilla_media::ShareOptions::default());
        ui.share_menu = true;
    });
    cx.update(|_, cx| cx.refresh_windows());
    cx.run_until_parked();
    assert_eq!(voice::frame_stat(1080, 30), "1080p · 30 fps");
}

/// Review C1: a call belongs to its instance window. Window B's syncs (every state its client publishes) never replace
/// window A's voice state, and one repaint loop runs per live call, ending when the call does or its window closes.
#[gpui::test]
fn another_window_s_sync_leaves_this_window_s_call_alone(cx: &mut TestAppContext) {
    cx.update(dilla_desktop::install);
    let a = cx.add_window(|_, _| Host);
    let b = cx.add_window(|_, _| Host);
    let (wa, wb): (WindowId, WindowId) = (a.window_id(), b.window_id());
    let mut idle = (*state(3)).clone();
    idle.voice.call = OwnCall::default();
    let idle = Arc::new(idle);
    let in_call = state(3);
    let loops = |cx: &mut TestAppContext| cx.update(|cx: &mut App| voice::repaint_loops(cx));
    let phase = |cx: &mut TestAppContext, w: WindowId| {
        cx.update(|cx: &mut App| voice::ui_of(w, cx).map(|u| u.state.voice.call.phase))
    };
    for _ in 0..10 {
        cx.update(|cx| voice::sync(wa, in_call.clone(), None, cx));
        cx.update(|cx| voice::sync(wb, idle.clone(), None, cx));
        cx.run_until_parked();
    }
    assert_eq!(phase(cx, wa), Some(CallPhase::In), "A's call stays A's");
    assert_eq!(phase(cx, wb), Some(CallPhase::Idle));
    assert_eq!(
        loops(cx),
        1,
        "one repaint loop for the one live call, however often the windows sync"
    );
    // The call ends: its loop ends with it.
    cx.update(|cx| voice::sync(wa, idle.clone(), None, cx));
    cx.executor()
        .advance_clock(std::time::Duration::from_millis(100));
    cx.run_until_parked();
    assert_eq!(loops(cx), 0);
    // A new call in A, then A closes: the loop ends, and A's area is let go at B's next sync.
    cx.update(|cx| voice::sync(wa, in_call.clone(), None, cx));
    assert_eq!(loops(cx), 1);
    a.update(cx, |_, window, _| window.remove_window()).unwrap();
    cx.run_until_parked();
    cx.update(|cx| voice::sync(wb, idle.clone(), None, cx));
    cx.executor()
        .advance_clock(std::time::Duration::from_millis(100));
    cx.run_until_parked();
    assert_eq!(loops(cx), 0);
    assert_eq!(phase(cx, wa), None, "a closed window's area is let go");
}

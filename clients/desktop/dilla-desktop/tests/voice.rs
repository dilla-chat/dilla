//! Task 17's tests for the voice area's views and keys (standard procedure S5-S6, the parts that run without a
//! network): the hooks draw from the voice slice, the stage's arrows, Enter and Escape move its selection and focus
//! mode, and the stage has a tile per stream. The parity tests (S4) and the two-device behaviour tests through
//! `Driver` wait on the shell's stage hook and the call seeding of the parity world (see the task report).

use std::rc::Rc;
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
        voice::ptt_keys(voice::wid(window), div())
            .size_full()
            .flex()
            .children(voice::channel_people(&ch, window, cx))
            .children(voice::dock(window, cx))
            .children(voice::stage(&ch, window, cx))
            .child(voice::ptt_root(voice::wid(window)))
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

// ---- with a real dillad: the window's call, its close, and one call per device ---------------------------------------

/// Review I1 (mic privacy): push to talk is listened for at the app's root, not in the shell's dock, and it is released
/// whenever the screen leaves the shell. Holding the key in a call, then opening the settings and letting go: the mic is
/// closed (it was held open while the settings replaced the dock, which alone listened).
#[cfg(feature = "automation")]
#[gpui::test]
fn push_to_talk_lets_go_when_the_settings_open(cx: &mut TestAppContext) {
    use dilla_desktop::app::{AppView, Launch};
    use dilla_desktop::router::Route;
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/fixtures/shell/channel-view.json");
    let loaded = dilla_desktop::fixture::Fixture::load(&path, None).expect("the shell fixture");
    cx.update(dilla_desktop::install);
    let (app, vcx) =
        cx.add_window_view(|window, cx| AppView::new(Launch::fixture(loaded), window, cx));
    vcx.run_until_parked();
    let w = vcx.update(|window, _| voice::wid(window));
    // This window's call is on, push to talk on F13.
    vcx.update(|_, cx| {
        voice::sync(w, state(2), None, cx);
        voice::set_ptt_key(w, Some(("f13".into(), 200)), cx);
    });
    vcx.update(|window, _| window.activate_window());
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    let f13 = gpui::Keystroke::parse("f13").unwrap();
    vcx.simulate_event(gpui::KeyDownEvent {
        keystroke: f13.clone(),
        is_held: false,
        prefer_character_input: false,
    });
    vcx.run_until_parked();
    assert!(
        vcx.update(|_, cx| voice::ptt_sending(w, cx)),
        "held: sending"
    );
    // The settings open over the shell (the gear): the key is let go of there.
    app.update_in(vcx, |a, window, cx| {
        a.open_route(&Route::Settings(None), window, cx)
    });
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    assert!(
        !vcx.update(|_, cx| voice::ptt_sending(w, cx)),
        "the screen left the shell: released"
    );
    vcx.simulate_event(gpui::KeyUpEvent { keystroke: f13 });
    vcx.run_until_parked();
    assert!(!vcx.update(|_, cx| voice::ptt_sending(w, cx)));
}

/// Review I1: the call ending releases push to talk.
#[gpui::test]
fn the_call_ending_releases_push_to_talk(cx: &mut TestAppContext) {
    let vcx = host(cx, state(2));
    let w = vcx.update(|window, _| voice::wid(window));
    vcx.update(|_, cx| voice::set_ptt_key(w, Some(("f13".into(), 200)), cx));
    // The stage holds the focus (a key goes down the focus path, under the window's root).
    let focus = stage_focus(vcx);
    vcx.update(|window, cx| focus.focus(window, cx));
    vcx.update(|window, _| window.activate_window());
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    vcx.simulate_event(gpui::KeyDownEvent {
        keystroke: gpui::Keystroke::parse("f13").unwrap(),
        is_held: false,
        prefer_character_input: false,
    });
    vcx.run_until_parked();
    assert!(vcx.update(|_, cx| voice::ptt_sending(w, cx)));
    vcx.update(|_, cx| voice::sync(w, Arc::new(AppState::default()), None, cx));
    assert!(
        !vcx.update(|_, cx| voice::ptt_sending(w, cx)),
        "the call ended"
    );
}

#[path = "../../dilla-client/tests/support/call_world.rs"]
mod call_world;

/// A real instance with a voice channel, alice and bob in its community, and a runtime for the waits.
struct CallWorld {
    rt: tokio::runtime::Runtime,
    _host: call_world::Host,
    alice: Rc<dilla_client::Client>,
    bob: Rc<dilla_client::Client>,
    voice: ChannelId,
}

const CALL_WAIT: std::time::Duration = std::time::Duration::from_secs(45);

fn call_world(tag: &'static str) -> CallWorld {
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let (host, alice, bob, voice) = rt.block_on(async {
        let host = tokio::task::spawn_blocking(move || call_world::start_host_reachable(tag))
            .await
            .unwrap();
        let invite = host.invites.last().unwrap().clone();
        let alice = call_world::sign_up(&host, &invite, "alice", tag).await;
        let bob = call_world::sign_up(&host, &invite, "bob", tag).await;
        let (host, alice, bob, voice) = tokio::task::spawn_blocking(move || {
            let (_, v) = call_world::voice_community(&host, &alice, &[&bob]);
            call_world::clear_session(&alice);
            call_world::clear_session(&bob);
            (host, alice, bob, ChannelId(v))
        })
        .await
        .unwrap();
        let a = call_world::start(&alice).await;
        call_world::connected(&a).await;
        a.voice().start();
        let b = call_world::start(&bob).await;
        call_world::connected(&b).await;
        b.voice().start();
        for c in [&a, &b] {
            until(c, "the voice channel is listed", |s| {
                s.channel(voice).is_some()
            })
            .await;
        }
        (host, a, b, voice)
    });
    CallWorld {
        rt,
        _host: host,
        alice: Rc::new(alice),
        bob: Rc::new(bob),
        voice,
    }
}

/// Waits until `pred` holds for `client`'s own state.
async fn until(
    client: &dilla_client::Client,
    what: &str,
    pred: impl Fn(&AppState) -> bool,
) -> Arc<AppState> {
    until_within(client, what, CALL_WAIT, pred).await
}

/// [`until`] within `wait`.
async fn until_within(
    client: &dilla_client::Client,
    what: &str,
    wait: std::time::Duration,
    pred: impl Fn(&AppState) -> bool,
) -> Arc<AppState> {
    let mut rx = client.state();
    let deadline = std::time::Instant::now() + wait;
    loop {
        let s = rx.borrow_and_update().clone();
        if pred(&s) {
            return s;
        }
        let left = deadline.saturating_duration_since(std::time::Instant::now());
        assert!(!left.is_zero(), "{what}: never; voice {:?}", s.voice);
        let _ = tokio::time::timeout(
            left.min(std::time::Duration::from_millis(500)),
            rx.changed(),
        )
        .await;
    }
}

fn phase_of(client: &dilla_client::Client) -> CallPhase {
    client.state().borrow().voice.call.phase
}

/// Opens a window whose voice area follows `client`, as its shell does.
fn window_for(
    cx: &mut TestAppContext,
    client: &Rc<dilla_client::Client>,
) -> gpui::WindowHandle<Host> {
    let w = cx.add_window(|_, _| Host);
    let id = w.window_id();
    let state = client.state().borrow().clone();
    cx.update(|cx| voice::sync(id, state, Some(client), cx));
    w
}

/// Review I4: closing the window of a call leaves the call at once (no other shell needs to sync): the media link is
/// cut, the client publishes Idle, and the window's media session is dropped.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn closing_the_window_leaves_its_call(cx: &mut TestAppContext) {
    let w = call_world("t-close-call");
    cx.update(dilla_desktop::install);
    // The app's close hook (installed with the manager, as the shipping app does).
    cx.update(|cx| {
        dilla_desktop::windows::open_manager(cx, &dilla_desktop::Launch::empty(), false)
            .expect("the manager opens")
    });
    let a = window_for(cx, &w.alice);
    let wa = a.window_id();
    cx.update(|cx| voice::join(w.voice, wa, cx));
    w.rt.block_on(until(&w.alice, "alice is in the call", |s| {
        s.voice.call.phase == CallPhase::In
    }));
    let session = cx
        .update(|cx: &mut App| voice::session_of(wa, cx))
        .map(|s| Rc::downgrade(&s))
        .expect("the window's media session");
    a.update(cx, |_, window, _| window.remove_window()).unwrap();
    cx.run_until_parked();
    // At once: the leave is the close's own, not the instance's eventual timeout of a silent device.
    w.rt.block_on(until_within(
        &w.alice,
        "the closed window's call is left at once",
        std::time::Duration::from_secs(3),
        |s| s.voice.call.phase == CallPhase::Idle,
    ));
    assert!(
        w.alice.voice().media().borrow().is_none(),
        "the media link is cut"
    );
    assert!(session.upgrade().is_none(), "the media session is dropped");
    assert!(cx.update(|cx: &mut App| voice::ui_of(wa, cx).is_none()));
}

/// Review N3: one call per device. Alice's window is in the call; a join in bob's window (another instance's) leaves
/// alice's call first, through alice's own client.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn a_join_in_another_window_leaves_this_window_s_call(cx: &mut TestAppContext) {
    let w = call_world("t-one-call");
    cx.update(dilla_desktop::install);
    let a = window_for(cx, &w.alice);
    let b = window_for(cx, &w.bob);
    let (wa, wb) = (a.window_id(), b.window_id());
    cx.update(|cx| voice::join(w.voice, wa, cx));
    w.rt.block_on(until(&w.alice, "alice is in the call", |s| {
        s.voice.call.phase == CallPhase::In
    }));
    assert_eq!(
        cx.update(|cx: &mut App| voice::client_of(wa, cx).map(|c| Rc::ptr_eq(&c, &w.alice))),
        Some(true),
        "A's area acts through A's client"
    );
    // B's join right away: B's window has not synced alice's new state, the client's own state says she is in.
    cx.update(|cx| voice::join(w.voice, wb, cx));
    w.rt.block_on(until(&w.alice, "alice's call is left", |s| {
        s.voice.call.phase == CallPhase::Idle
    }));
    w.rt.block_on(until(&w.bob, "bob is in the call", |s| {
        s.voice.call.phase == CallPhase::In
    }));
    assert_eq!(phase_of(&w.alice), CallPhase::Idle);
}

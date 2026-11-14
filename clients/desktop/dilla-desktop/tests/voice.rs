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
    App, Context, FocusHandle, InteractiveElement, IntoElement, ParentElement, Render, Styled,
    TestAppContext, VisualTestContext, Window, WindowId, div,
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

/// A window with a text field (as the composer is) under the app's root push-to-talk handlers.
struct FieldHost {
    field: gpui::Entity<gpui_component::input::InputState>,
    root: FocusHandle,
}

impl Render for FieldHost {
    fn render(&mut self, window: &mut Window, _cx: &mut Context<Self>) -> impl IntoElement {
        voice::ptt_keys(voice::wid(window), div().track_focus(&self.root))
            .size_full()
            .child(gpui_component::input::Input::new(&self.field))
    }
}

/// A call on, push to talk on Insert, a text field in the window; answers the window's id and the field's focus.
fn field_host(cx: &mut TestAppContext) -> (&mut VisualTestContext, WindowId, FocusHandle) {
    use gpui::AppContext as _;
    cx.update(dilla_desktop::install);
    let (host, vcx) = cx.add_window_view(|window, cx| FieldHost {
        field: cx.new(|cx| gpui_component::input::InputState::new(window, cx)),
        root: cx.focus_handle(),
    });
    let w = vcx.update(|window, _| voice::wid(window));
    vcx.update(|_, cx| {
        voice::sync(w, state(2), None, cx);
        voice::set_ptt_key(w, Some(("insert".into(), 200)), cx);
    });
    vcx.update(|window, _| window.activate_window());
    let field = host.read_with(vcx, |h, cx| {
        gpui::Focusable::focus_handle(h.field.read(cx), cx)
    });
    let root = host.read_with(vcx, |h, _| h.root.clone());
    vcx.update(|window, cx| root.focus(window, cx));
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    (vcx, w, field)
}

fn key_down(vcx: &mut VisualTestContext, key: &'static str) {
    vcx.simulate_event(gpui::KeyDownEvent {
        keystroke: gpui::Keystroke::parse(key).unwrap(),
        is_held: false,
        prefer_character_input: false,
    });
    vcx.run_until_parked();
}

/// Review A (web ptt.ts): Shift+Insert (paste) in a text field never opens the mic.
#[gpui::test]
fn push_to_talk_ignores_a_modified_key(cx: &mut TestAppContext) {
    let (vcx, w, field) = field_host(cx);
    vcx.update(|window, cx| field.focus(window, cx));
    vcx.run_until_parked();
    key_down(vcx, "shift-insert");
    assert!(
        !vcx.update(|_, cx| voice::ptt_sending(w, cx)),
        "a modifier: not push to talk"
    );
}

/// Review A (web ptt.ts): the key types as usual in a text field; the mic stays closed.
#[gpui::test]
fn push_to_talk_is_off_in_a_text_field(cx: &mut TestAppContext) {
    let (vcx, w, field) = field_host(cx);
    vcx.update(|window, cx| field.focus(window, cx));
    vcx.run_until_parked();
    key_down(vcx, "insert");
    assert!(
        !vcx.update(|_, cx| voice::ptt_sending(w, cx)),
        "in a text field: not push to talk"
    );
}

/// Review A: the key with the focus outside any text field opens the mic.
#[gpui::test]
fn push_to_talk_opens_the_mic_outside_text_fields(cx: &mut TestAppContext) {
    let (vcx, w, _) = field_host(cx);
    // The focus is on the window's root (no text field): `field_host` put it there.
    key_down(vcx, "insert");
    assert!(
        vcx.update(|_, cx| voice::ptt_sending(w, cx)),
        "held: sending"
    );
}

/// A window whose focused control can go away mid-hold, under the root's push-to-talk handlers and focus guard.
struct GoneHost {
    root: FocusHandle,
    control: FocusHandle,
    show: bool,
    _lost: Option<gpui::Subscription>,
}

impl GoneHost {
    /// As the app's root: its focus guard at render and its focus-lost subscription.
    fn new(show: bool, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let lost = cx.on_focus_lost(window, |this: &mut Self, window, cx| {
            let root = this.root.clone();
            voice::focus_lost(voice::wid(window), &root, window, cx);
        });
        Self {
            root: cx.focus_handle(),
            control: cx.focus_handle(),
            show,
            _lost: Some(lost),
        }
    }
}

impl Render for GoneHost {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let w = voice::wid(window);
        voice::guard_focus(w, &self.root, window, cx);
        voice::ptt_keys(w, div().track_focus(&self.root))
            .size_full()
            .children(
                self.show
                    .then(|| div().track_focus(&self.control).size_full()),
            )
    }
}

/// Review B: the focused control goes away while the key is held (GPUI keeps a stale focus, and a key then reaches no
/// handler of ours): push to talk lets go, and the key-up after it changes nothing.
#[gpui::test]
fn a_focused_control_going_away_mid_hold_closes_the_mic(cx: &mut TestAppContext) {
    cx.update(dilla_desktop::install);
    let (host, vcx) = cx.add_window_view(|window, cx| GoneHost::new(true, window, cx));
    let w = vcx.update(|window, _| voice::wid(window));
    vcx.update(|_, cx| {
        voice::sync(w, state(2), None, cx);
        voice::set_ptt_key(w, Some(("insert".into(), 200)), cx);
    });
    vcx.update(|window, _| window.activate_window());
    let control = host.read_with(vcx, |h, _| h.control.clone());
    vcx.update(|window, cx| control.focus(window, cx));
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    key_down(vcx, "insert");
    assert!(
        vcx.update(|_, cx| voice::ptt_sending(w, cx)),
        "held: sending"
    );
    host.update(vcx, |h, cx| {
        h.show = false;
        cx.notify();
    });
    vcx.run_until_parked();
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    vcx.simulate_event(gpui::KeyUpEvent {
        keystroke: gpui::Keystroke::parse("insert").unwrap(),
    });
    vcx.run_until_parked();
    assert!(
        !vcx.update(|_, cx| voice::ptt_sending(w, cx)),
        "the mic is closed"
    );
}

/// Review B: the window going inactive lets push to talk go at once, from the window's activation observer (no frame
/// needed: a hidden Wayland surface gets none).
#[cfg(feature = "automation")]
#[gpui::test]
fn the_window_deactivating_closes_the_mic(cx: &mut TestAppContext) {
    use dilla_desktop::app::{AppView, Launch};
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/fixtures/shell/channel-view.json");
    let loaded = dilla_desktop::fixture::Fixture::load(&path, None).expect("the shell fixture");
    cx.update(dilla_desktop::install);
    let (_app, vcx) =
        cx.add_window_view(|window, cx| AppView::new(Launch::fixture(loaded), window, cx));
    let w = vcx.update(|window, _| voice::wid(window));
    vcx.update(|_, cx| {
        voice::sync(w, state(2), None, cx);
        voice::set_ptt_key(w, Some(("insert".into(), 200)), cx);
    });
    vcx.update(|window, _| window.activate_window());
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    key_down(vcx, "insert");
    assert!(
        vcx.update(|_, cx| voice::ptt_sending(w, cx)),
        "held: sending"
    );
    vcx.deactivate_window();
    // At once, from the observer: no frame is drawn in between.
    assert!(!vcx.update(|_, cx| voice::ptt_sending(w, cx)), "released");
}

/// Re-review B2: in a call, a control mounted and focused in the same update (a dialog's field, the settings host, a
/// recreated composer) keeps the focus: the guard never takes it back before that control was drawn.
#[gpui::test]
fn a_new_control_focused_in_a_call_keeps_the_focus(cx: &mut TestAppContext) {
    cx.update(dilla_desktop::install);
    let (host, vcx) = cx.add_window_view(|window, cx| GoneHost::new(false, window, cx));
    let w = vcx.update(|window, _| voice::wid(window));
    vcx.update(|_, cx| {
        voice::sync(w, state(2), None, cx);
        voice::set_ptt_key(w, Some(("insert".into(), 200)), cx);
    });
    vcx.update(|window, _| window.activate_window());
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    // The control mounts and takes the focus in one update, before any frame drew it.
    let control = host.read_with(vcx, |h, _| h.control.clone());
    host.update_in(vcx, |h, window, cx| {
        h.show = true;
        control.focus(window, cx);
        cx.notify();
    });
    for _ in 0..3 {
        vcx.run_until_parked();
        vcx.update(|window, _| window.refresh());
    }
    vcx.run_until_parked();
    assert!(
        vcx.update(|window, _| control.is_focused(window)),
        "the new control keeps the focus"
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
    /// Declared last, so it is let go after everything above.
    _media: MediaSlot,
}

const CALL_WAIT: std::time::Duration = std::time::Duration::from_secs(45);

/// The process's one media slot. `dilla_media`'s frame hook routes one live call per process (N2): a second window's
/// media in the same process fails with `E_CALL_BUSY`, and the desktop then leaves that call (its media did not
/// start). The real-instance tests here run on parallel threads of one test binary, so a test whose media lost the
/// slot to another test's left its own call; alone in it, the leave ended the call and closed its group, and bob's join
/// built on that group was refused (`E_COMMIT_INVALID`: the delivery service refuses a commit to a closed group). Each
/// real-instance test holds the slot for its whole run, and first waits for an earlier test's media to be gone (a
/// window's media session can outlive its test's world).
static MEDIA_SLOT: std::sync::Mutex<()> = std::sync::Mutex::new(());

/// How long a test waits for an earlier test's call media to end.
const MEDIA_FREE_WAIT: std::time::Duration = std::time::Duration::from_secs(30);

/// A held [`MEDIA_SLOT`].
struct MediaSlot {
    _guard: std::sync::MutexGuard<'static, ()>,
}

/// Takes the process's media slot ([`MEDIA_SLOT`]) once no call media is live in this process.
fn media_slot() -> MediaSlot {
    let guard = MEDIA_SLOT.lock().unwrap_or_else(|p| p.into_inner());
    assert!(
        media_free_within(MEDIA_FREE_WAIT),
        "an earlier test's call media is still live in this process after {MEDIA_FREE_WAIT:?}"
    );
    MediaSlot { _guard: guard }
}

/// True once no call is current in this process's media router, polled until `wait` has passed. The probe is the
/// router's own claim, released at once: only a holder of [`MEDIA_SLOT`] probes, and every test of this binary that
/// starts call media holds it.
fn media_free_within(wait: std::time::Duration) -> bool {
    let router = dilla_media::transform::Router::get().expect("the dilla frame hook installs");
    let deadline = std::time::Instant::now() + wait;
    loop {
        let probe = dilla_media::transform::CallMedia::new();
        match router.begin(probe.clone()) {
            Ok(()) => {
                router.end(&probe);
                return true;
            }
            Err(dilla_media::MediaError::Busy) => {}
            Err(e) => panic!("the media router refused the probe: {e}"),
        }
        if std::time::Instant::now() >= deadline {
            return false;
        }
        std::thread::sleep(std::time::Duration::from_millis(50));
    }
}

/// The race the slot closes, forced: a call live in this process (another test's window) holds the next real-instance
/// test, instead of that test's media starting into `E_CALL_BUSY`; the wait ends once the live call's media ends.
#[test]
fn a_live_call_in_the_process_holds_the_next_call_test_until_its_media_ends() {
    let _slot = MEDIA_SLOT.lock().unwrap_or_else(|p| p.into_inner());
    let router = dilla_media::transform::Router::get().expect("the dilla frame hook installs");
    let live = dilla_media::transform::CallMedia::new();
    router.begin(live.clone()).expect("no call is live yet");
    // The race itself: a second call's media cannot start while the first is live.
    assert!(matches!(
        router.begin(dilla_media::transform::CallMedia::new()),
        Err(dilla_media::MediaError::Busy)
    ));
    assert!(
        !media_free_within(std::time::Duration::from_millis(300)),
        "a live call holds the slot"
    );
    let waiter = std::thread::spawn(|| media_free_within(std::time::Duration::from_secs(10)));
    std::thread::sleep(std::time::Duration::from_millis(300));
    assert!(
        !waiter.is_finished(),
        "the next test waits while the call is live"
    );
    router.end(&live);
    assert!(
        waiter.join().unwrap(),
        "the wait ends with the call's media"
    );
    assert!(
        media_free_within(std::time::Duration::ZERO),
        "the slot is free again"
    );
}

fn call_world(tag: &'static str) -> CallWorld {
    let media = media_slot();
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
        _media: media,
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

/// Waits until `pred` holds for `client`'s own state while running the UI's executor (a join's handover runs there,
/// polling the other windows' leaves on a timer): the test clock moves as fast as the real one, so the handover's bound
/// is the real time the clients take. Between the UI's turns every state change is looked at (a phase can be brief).
fn pump(
    cx: &mut TestAppContext,
    rt: &tokio::runtime::Runtime,
    client: &dilla_client::Client,
    what: &str,
    pred: impl Fn(&AppState) -> bool,
) {
    let deadline = std::time::Instant::now() + CALL_WAIT;
    let mut rx = client.state();
    let mut moved = std::time::Instant::now();
    loop {
        cx.run_until_parked();
        let seen = rt.block_on(async {
            let turn = std::time::Instant::now() + std::time::Duration::from_millis(30);
            loop {
                if pred(&rx.borrow_and_update().clone()) {
                    return true;
                }
                let left = turn.saturating_duration_since(std::time::Instant::now());
                if left.is_zero() {
                    return false;
                }
                let _ = tokio::time::timeout(left, rx.changed()).await;
            }
        });
        if seen {
            return;
        }
        assert!(
            std::time::Instant::now() < deadline,
            "{what}: never; voice {:?}",
            client.state().borrow().voice
        );
        cx.executor().advance_clock(moved.elapsed());
        moved = std::time::Instant::now();
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
    // No other call: the handover starts the join in its first turn.
    cx.run_until_parked();
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
    // No other call: the handover starts the join in its first turn.
    cx.run_until_parked();
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
    pump(cx, &w.rt, &w.bob, "bob is in the call", |s| {
        s.voice.call.phase == CallPhase::In
    });
    assert_eq!(phase_of(&w.alice), CallPhase::Idle);
}

/// Task 28 item 3: quitting the app leaves the live calls (the leave sends the voice Remove to the instance), waiting
/// for the leaves a bounded time: the leave has answered once the quit goes on.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn quitting_leaves_the_live_call(cx: &mut TestAppContext) {
    let w = call_world("t-quit-call");
    cx.update(dilla_desktop::install);
    let a = window_for(cx, &w.alice);
    let wa = a.window_id();
    cx.update(|cx| voice::join(w.voice, wa, cx));
    // No other call: the handover starts the join in its first turn.
    cx.run_until_parked();
    w.rt.block_on(until(&w.alice, "alice is in the call", |s| {
        s.voice.call.phase == CallPhase::In
    }));
    assert!(
        cx.update(voice::on_quit),
        "the call's leave answered within the quit's wait"
    );
    assert_eq!(phase_of(&w.alice), CallPhase::Idle);
    assert!(
        w.alice.voice().media().borrow().is_none(),
        "the media link is cut"
    );
}

/// Task 28 item 3: the quit's wait for the leaves is bounded: a leave that never answers does not hold the quit.
#[test]
fn the_quit_s_wait_for_the_leaves_is_bounded() {
    let (_tx, never) = answer_pair();
    let started = std::time::Instant::now();
    assert!(!voice::wait_answers(
        vec![never],
        std::time::Duration::from_millis(150)
    ));
    let took = started.elapsed();
    assert!(
        took >= std::time::Duration::from_millis(150) && took < std::time::Duration::from_secs(2)
    );
    let (tx, answered) = answer_pair();
    tx.send(Ok(())).unwrap();
    let (tx2, closed) = answer_pair();
    drop(tx2);
    assert!(voice::wait_answers(
        vec![answered, closed],
        std::time::Duration::from_secs(5)
    ));
}

// ---- the handover: another window's leave, then this window's join (Task 28 items 1 and 2) ------------------------

type Answer = tokio::sync::oneshot::Sender<Result<(), dilla_client::ClientError>>;
type Answered = tokio::sync::oneshot::Receiver<Result<(), dilla_client::ClientError>>;

fn answer_pair() -> (Answer, Answered) {
    tokio::sync::oneshot::channel()
}

/// A window with a voice area, as its shell syncs it.
fn handover_window(cx: &mut TestAppContext) -> (gpui::WindowHandle<Host>, WindowId) {
    cx.update(dilla_desktop::install);
    let w = cx.add_window(|_, _| Host);
    let id = w.window_id();
    cx.update(|cx| voice::sync(id, state(1), None, cx));
    (w, id)
}

/// What the handover did: started the join, undid it.
#[derive(Default)]
struct Did {
    started: std::cell::Cell<bool>,
    undone: std::cell::Cell<bool>,
}

fn start_and_undo(
    did: &Rc<Did>,
    join: Answered,
) -> (impl FnOnce() -> Answered + 'static, impl FnOnce() + 'static) {
    let (a, b) = (did.clone(), did.clone());
    (
        move || {
            a.started.set(true);
            join
        },
        move || b.undone.set(true),
    )
}

fn toast_in(cx: &mut TestAppContext, id: WindowId) -> Option<String> {
    cx.update(|cx| dilla_desktop_ui::components::toast::current_in(id, cx))
        .map(|t| t.to_string())
}

/// Task 28 item 1: another window's leave that never answers holds the join for a bounded time only; then the join is
/// dropped and the window says it could not join.
#[gpui::test]
fn a_leave_that_never_answers_ends_the_handover_with_an_error(cx: &mut TestAppContext) {
    let (_w, id) = handover_window(cx);
    let did = Rc::new(Did::default());
    let (_leave_tx, leave_rx) = answer_pair();
    let (_join_tx, join_rx) = answer_pair();
    let (start, undo) = start_and_undo(&did, join_rx);
    cx.update(|cx| voice::handover(id, vec![leave_rx], start, undo, cx));
    cx.run_until_parked();
    cx.executor()
        .advance_clock(voice::HANDOVER_WAIT - std::time::Duration::from_secs(1));
    cx.run_until_parked();
    assert!(!did.started.get(), "the join waits for the leave");
    assert_eq!(toast_in(cx, id), None);
    cx.executor()
        .advance_clock(std::time::Duration::from_secs(2));
    cx.run_until_parked();
    assert!(
        !did.started.get(),
        "no join after a leave that never answered"
    );
    assert_eq!(
        toast_in(cx, id).as_deref(),
        Some(dilla_desktop::strings::t("voice.failed")),
        "the window says the join failed"
    );
}

/// Task 28 item 1: the leave answering starts the join; a join that errors is not silent.
#[gpui::test]
fn a_join_error_after_the_handover_is_shown(cx: &mut TestAppContext) {
    let (_w, id) = handover_window(cx);
    let did = Rc::new(Did::default());
    let (leave_tx, leave_rx) = answer_pair();
    let (join_tx, join_rx) = answer_pair();
    let (start, undo) = start_and_undo(&did, join_rx);
    cx.update(|cx| voice::handover(id, vec![leave_rx], start, undo, cx));
    cx.run_until_parked();
    assert!(!did.started.get());
    leave_tx.send(Ok(())).unwrap();
    cx.executor()
        .advance_clock(std::time::Duration::from_millis(200));
    cx.run_until_parked();
    assert!(did.started.get(), "the join starts once the leave answered");
    join_tx
        .send(Err(dilla_client::ClientError::UnknownChannel))
        .unwrap();
    cx.executor()
        .advance_clock(std::time::Duration::from_millis(200));
    cx.run_until_parked();
    assert_eq!(
        toast_in(cx, id).as_deref(),
        Some(dilla_desktop::strings::t("voice.failed"))
    );
    assert!(!did.undone.get());
}

/// Task 28 item 2: a window that closes while another window's leave is pending starts no join.
#[gpui::test]
fn a_window_closed_before_the_handover_starts_no_join(cx: &mut TestAppContext) {
    let (w, id) = handover_window(cx);
    let did = Rc::new(Did::default());
    let (leave_tx, leave_rx) = answer_pair();
    let (_join_tx, join_rx) = answer_pair();
    let (start, undo) = start_and_undo(&did, join_rx);
    cx.update(|cx| voice::handover(id, vec![leave_rx], start, undo, cx));
    cx.run_until_parked();
    w.update(cx, |_, window, _| window.remove_window()).unwrap();
    cx.update(|cx| voice::window_closed(id, cx));
    leave_tx.send(Ok(())).unwrap();
    cx.executor()
        .advance_clock(std::time::Duration::from_millis(200));
    cx.run_until_parked();
    assert!(!did.started.get(), "no join for a closed window");
}

/// Task 28 item 2: a join that lands after its window closed is left at once (else the client stays in a call with no
/// voice area, the mic closed).
#[gpui::test]
fn a_join_that_lands_after_its_window_closed_is_left(cx: &mut TestAppContext) {
    let (w, id) = handover_window(cx);
    let did = Rc::new(Did::default());
    let (join_tx, join_rx) = answer_pair();
    let (start, undo) = start_and_undo(&did, join_rx);
    cx.update(|cx| voice::handover(id, Vec::new(), start, undo, cx));
    cx.run_until_parked();
    assert!(did.started.get(), "no other call: the join starts at once");
    w.update(cx, |_, window, _| window.remove_window()).unwrap();
    cx.update(|cx| voice::window_closed(id, cx));
    join_tx.send(Ok(())).unwrap();
    cx.executor()
        .advance_clock(std::time::Duration::from_millis(200));
    cx.run_until_parked();
    assert!(did.undone.get(), "the late join is left");
}

/// Task 28 item 2: a join that lands while its window is open stays.
#[gpui::test]
fn a_join_that_lands_in_an_open_window_stays(cx: &mut TestAppContext) {
    let (_w, id) = handover_window(cx);
    let did = Rc::new(Did::default());
    let (join_tx, join_rx) = answer_pair();
    let (start, undo) = start_and_undo(&did, join_rx);
    cx.update(|cx| voice::handover(id, Vec::new(), start, undo, cx));
    join_tx.send(Ok(())).unwrap();
    cx.executor()
        .advance_clock(std::time::Duration::from_millis(200));
    cx.run_until_parked();
    assert!(did.started.get());
    assert!(!did.undone.get());
    assert_eq!(toast_in(cx, id), None);
}

/// Alice's device's flags in the voice channel's roster as `client` holds it (0 when she is not listed).
fn flags_of(client: &dilla_client::Client, voice: ChannelId, device: [u8; 16]) -> u8 {
    client
        .state()
        .borrow()
        .voice
        .participants(voice)
        .iter()
        .find(|p| p.device_id == DeviceId(device))
        .map_or(0, |p| p.flags)
}

/// e2e/voice/controls.spec.ts, "Control+Shift+M and Control+Shift+D show alice muted and deafened to bob; push to talk
/// sends only while held" (the shortcut half; push to talk is held by the tests above): in a call with bob, alice's
/// window takes Ctrl+Shift+M and bob's roster shows her muted; again, unmuted. Ctrl+Shift+D deafens her as bob sees it,
/// and again undeafens.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn control_shift_m_and_d_show_muted_and_deafened_to_the_peer(cx: &mut TestAppContext) {
    let w = call_world("t28-mute-keys");
    cx.update(dilla_desktop::install);
    let a = window_for(cx, &w.alice);
    let wa = a.window_id();
    cx.update(|cx| voice::join(w.voice, wa, cx));
    // No other call: the handover starts the join in its first turn.
    cx.run_until_parked();
    w.rt.block_on(until(&w.alice, "alice is in the call", |s| {
        s.voice.call.phase == CallPhase::In
    }));
    w.rt.block_on(w.bob.voice().join(w.voice))
        .unwrap()
        .expect("bob joins");
    let alice_device = w.alice.state().borrow().account.device_id.unwrap().0;
    w.rt.block_on(until(&w.bob, "bob lists alice in the call", |s| {
        s.voice
            .participants(w.voice)
            .iter()
            .any(|p| p.device_id == DeviceId(alice_device))
    }));
    // Each press reads the window's view of the call, as the shell keeps it in sync with the client.
    let sync = |cx: &mut TestAppContext| {
        let state = w.alice.state().borrow().clone();
        cx.update(|cx| voice::sync(wa, state, Some(&w.alice), cx));
        cx.run_until_parked();
    };
    let press_and_see = |cx: &mut TestAppContext, key: &str, bit: u8, on: bool, what: &str| {
        sync(cx);
        cx.simulate_keystrokes(a.into(), key);
        cx.run_until_parked();
        w.rt.block_on(until(&w.bob, what, |_| {
            (flags_of(&w.bob, w.voice, alice_device) & bit != 0) == on
        }));
        // The client's own state follows too, so the next press reads it.
        w.rt.block_on(until(&w.alice, what, |s| {
            s.voice
                .participants(w.voice)
                .iter()
                .any(|p| p.own && (p.flags & bit != 0) == on)
        }));
    };
    assert_eq!(
        flags_of(&w.bob, w.voice, alice_device) & flags::SELF_MUTE,
        0
    );
    press_and_see(
        cx,
        "ctrl-shift-m",
        flags::SELF_MUTE,
        true,
        "bob sees alice muted",
    );
    press_and_see(
        cx,
        "ctrl-shift-m",
        flags::SELF_MUTE,
        false,
        "bob sees alice unmuted",
    );
    press_and_see(
        cx,
        "ctrl-shift-d",
        flags::SELF_DEAF,
        true,
        "bob sees alice deafened",
    );
    press_and_see(
        cx,
        "ctrl-shift-d",
        flags::SELF_DEAF,
        false,
        "bob sees alice undeafened",
    );
}

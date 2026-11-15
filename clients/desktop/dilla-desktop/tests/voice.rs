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

/// Package 8b (VoiceView.tsx `personMenu`, Signal §5.4): a right-click on another person's tile opens their menu (their
/// volume, Profile, Message, Mute for me, Copy username); Shift+F10 on the own tile opens yours (Mute, Deafen, Profile,
/// Move to the server's other voice channel with its head count, the AFK channel left out, and Leave voice). Escape closes.
#[gpui::test]
fn the_tile_menu_opens_with_the_person_s_items(cx: &mut TestAppContext) {
    let mut s = (*state(3)).clone();
    let mut other = channel();
    other.id = ChannelId([10; 16]);
    other.name = "Upstairs".into();
    s.channels.push(other);
    let cx = host(cx, Arc::new(s));
    let tiles = area(cx, |ui| voice::tiles(&ui.state, VOICE));
    let bounds = area(cx, |ui| ui.menu.bounds.borrow().get(&tiles[1]).copied())
        .expect("person 1's tile was laid out");
    cx.simulate_event(gpui::MouseDownEvent {
        button: gpui::MouseButton::Right,
        position: bounds.center(),
        modifiers: gpui::Modifiers::none(),
        click_count: 1,
        first_mouse: false,
    });
    cx.run_until_parked();
    assert!(cx.update(|window, cx| voice::menu::is_open(voice::wid(window), cx)));
    for item in [
        "menu-item-profile",
        "menu-item-message",
        "menu-item-mute-for-me",
        "menu-item-copy",
    ] {
        assert!(cx.debug_bounds(item).is_some(), "{item} is offered");
    }
    assert!(
        cx.debug_bounds("menu-item-leave").is_none(),
        "Leave voice is your own"
    );
    press(cx, "escape");
    assert!(!cx.update(|window, cx| voice::menu::is_open(voice::wid(window), cx)));

    // Your own tile, from the keyboard.
    let focus = stage_focus(cx);
    cx.update(|window, cx| focus.focus(window, cx));
    cx.run_until_parked();
    assert_eq!(area(cx, |ui| ui.selected), 0);
    press(cx, "shift-f10");
    assert!(cx.update(|window, cx| voice::menu::is_open(voice::wid(window), cx)));
    let upstairs: &'static str = format!("menu-item-move:{}", ChannelId([10; 16]).hex()).leak();
    for item in [
        "menu-item-mute",
        "menu-item-deafen",
        "menu-item-profile",
        upstairs,
        "menu-item-leave",
    ] {
        assert!(cx.debug_bounds(item).is_some(), "{item} is offered");
    }
    assert!(cx.debug_bounds("menu-item-mute-for-me").is_none());
}

/// Package 8c (CallTile.tsx `__tools`): the focused tile offers Grid, which goes back to the grid, and full screen; a
/// tile in the grid offers neither.
#[gpui::test]
fn the_focused_tile_offers_grid_and_full_screen(cx: &mut TestAppContext) {
    let cx = host(cx, state(3));
    assert!(
        cx.debug_bounds("voice-tile-grid").is_none(),
        "the grid has no tools"
    );
    let focus = stage_focus(cx);
    cx.update(|window, cx| focus.focus(window, cx));
    press(cx, "enter");
    assert!(area(cx, |ui| ui.focused.is_some()));
    assert!(cx.debug_bounds("voice-tile-fullscreen").is_some());
    let grid = cx.debug_bounds("voice-tile-grid").expect("Grid").center();
    cx.simulate_click(grid, gpui::Modifiers::none());
    cx.run_until_parked();
    assert_eq!(area(cx, |ui| ui.focused.clone()), None, "back to the grid");
}

/// Voice-2 (CallTile.tsx `toggleFullscreen`): the tile's full screen makes the window full screen with that tile alone
/// in it; Escape gives the stage back and the window leaves full screen. A window already full screen (the app's own full
/// screen, layout unchanged) stays so when the tile leaves.
#[gpui::test]
fn a_tile_s_full_screen_shows_only_it_and_escape_returns(cx: &mut TestAppContext) {
    let cx = host(cx, state(3));
    let focus = stage_focus(cx);
    cx.update(|window, cx| focus.focus(window, cx));
    press(cx, "enter");
    // The tool's action (this host's focused tile runs past the window's edge, so the action is taken directly).
    assert!(cx.debug_bounds("voice-tile-fullscreen").is_some());
    let key = area(cx, |ui| ui.focused.clone()).expect("a focused tile");
    let toggle = |cx: &mut VisualTestContext| {
        let key = key.clone();
        cx.update(|window, cx| voice::toggle_tile_fullscreen(key, window, cx));
        cx.run_until_parked();
    };
    toggle(cx);
    let w = cx.update(|window, _| voice::wid(window));
    assert!(cx.update(|_, cx| voice::fullscreen_tile(w, cx)).is_some());
    assert!(
        cx.update(|window, _| window.is_fullscreen()),
        "the window went full screen for the tile"
    );
    press(cx, "escape");
    assert!(cx.update(|_, cx| voice::fullscreen_tile(w, cx)).is_none());
    assert!(
        !cx.update(|window, _| window.is_fullscreen()),
        "and back out"
    );

    // The app's own full screen first: the tile's leaves it as it was.
    cx.update(|window, _| window.toggle_fullscreen());
    toggle(cx);
    assert!(cx.update(|_, cx| voice::fullscreen_tile(w, cx)).is_some());
    press(cx, "escape");
    assert!(
        cx.update(|window, _| window.is_fullscreen()),
        "the app's full screen stays"
    );
}

/// Voice-2: a tile shown full screen whose client is released (sign-out, Forget) gives the window back out of full
/// screen, though nothing draws the stage any more.
#[gpui::test]
fn releasing_the_client_mid_full_screen_restores_the_window(cx: &mut TestAppContext) {
    let cx = host(cx, state(3));
    let focus = stage_focus(cx);
    cx.update(|window, cx| focus.focus(window, cx));
    press(cx, "enter");
    let key = area(cx, |ui| ui.focused.clone()).unwrap();
    cx.update(|window, cx| voice::toggle_tile_fullscreen(key, window, cx));
    cx.run_until_parked();
    assert!(cx.update(|window, _| window.is_fullscreen()));
    // The area goes as a released client's does (no client in this host: the window's area is let go directly).
    let w = cx.update(|window, _| voice::wid(window));
    cx.update(|_, cx| voice::window_released_for_tests(w, cx));
    cx.run_until_parked();
    assert!(
        !cx.update(|window, _| window.is_fullscreen()),
        "the window is back"
    );
}

/// Voice-2 (Shell.tsx `onProfile`, ProfileCard `placement="below"`): the person menu's Profile opens the profile card
/// under the tile it was opened on, not the member list's.
#[gpui::test]
fn profile_opens_the_card_under_the_tile(cx: &mut TestAppContext) {
    let mut s = (*state(3)).clone();
    s.communities.push(dilla_client::state::Community {
        id: COMMUNITY,
        name: "Midgard".into(),
        owner: UserId([0; 16]),
        policy_version: 0,
    });
    let cx = host(cx, Arc::new(s));
    let tiles = area(cx, |ui| voice::tiles(&ui.state, VOICE));
    let tile = area(cx, |ui| ui.menu.bounds.borrow().get(&tiles[1]).copied()).unwrap();
    cx.simulate_event(gpui::MouseDownEvent {
        button: gpui::MouseButton::Right,
        position: tile.center(),
        modifiers: gpui::Modifiers::none(),
        click_count: 1,
        first_mouse: false,
    });
    cx.run_until_parked();
    let item = cx
        .debug_bounds("menu-item-profile")
        .expect("Profile")
        .center();
    cx.simulate_click(item, gpui::Modifiers::none());
    cx.run_until_parked();
    let card = cx.debug_bounds("voice-profile-card").expect("the card");
    let viewport = cx.update(|window, _| window.viewport_size());
    assert!(
        card.top() >= tile.bottom()
            || card.bottom() <= tile.top()
            || card.bottom() >= viewport.height - gpui::px(9.0),
        "beside the tile: {card:?} / {tile:?}"
    );
    // A press outside closes it.
    cx.simulate_mouse_down(
        gpui::point(gpui::px(4.0), gpui::px(4.0)),
        gpui::MouseButton::Left,
        gpui::Modifiers::none(),
    );
    cx.run_until_parked();
    assert!(
        cx.debug_bounds("voice-profile-card").is_none(),
        "a press outside closes it"
    );
}

/// Package 8c (VoiceChannelRow.tsx): a bot in a voice channel's row says "playback only" after its name; a person does not.
#[gpui::test]
fn a_bot_s_voice_row_says_playback_only(cx: &mut TestAppContext) {
    let cx = host(cx, state(3));
    assert!(cx.debug_bounds("voice-person-playback-only").is_none());
    let mut s = (*state(3)).clone();
    if let Some(m) = s.members.get_mut(&COMMUNITY) {
        m[2].kind = 1;
    }
    cx.update(|window, cx| {
        voice::sync(voice::wid(window), Arc::new(s), None, cx);
        window.refresh();
    });
    cx.run_until_parked();
    assert!(cx.debug_bounds("voice-person-playback-only").is_some());
}

/// Package 8c (UserPanel.tsx `ptt`): in push-to-talk mode the mute button carries the PTT tag; otherwise it has none.
#[gpui::test]
fn the_user_panel_tags_push_to_talk(cx: &mut TestAppContext) {
    use dilla_desktop::screens::user_panel::{PanelHandlers, PanelModel, render};
    struct Panel(Option<bool>);
    impl Render for Panel {
        fn render(&mut self, _: &mut Window, _: &mut Context<Self>) -> impl IntoElement {
            let handlers = PanelHandlers {
                on_mute: Rc::new(|_, _| {}),
                on_deafen: Rc::new(|_, _| {}),
            };
            div().size_full().child(render(
                &PanelModel {
                    name: "Ada",
                    status: "online",
                    in_room: None,
                    muted: false,
                    deafened: false,
                    ptt: self.0,
                },
                &handlers,
            ))
        }
    }
    let (_, vcx) = cx.add_window_view(|_, _| Panel(Some(false)));
    vcx.run_until_parked();
    assert!(vcx.debug_bounds("user-panel-ptt").is_some());
    let (_, vcx) = cx.add_window_view(|_, _| Panel(None));
    vcx.run_until_parked();
    assert!(vcx.debug_bounds("user-panel-ptt").is_none());
}

/// Opens the user settings over `s` in a window of its own, with the routes it asks for.
fn open_settings(
    cx: &mut TestAppContext,
    s: Arc<AppState>,
) -> (
    &mut VisualTestContext,
    Rc<std::cell::RefCell<Vec<dilla_desktop::router::Route>>>,
) {
    use dilla_desktop::screens::settings_host::{SettingsEvent, UserSettingsHost};
    let ctx = dilla_desktop::slots::SlotCx {
        state: s,
        ..dilla_desktop::slots::SlotCx::default()
    };
    let (host, vcx) = cx.add_window_view(|_, cx| UserSettingsHost::new(None, None, ctx, cx));
    let routes = Rc::new(std::cell::RefCell::new(Vec::new()));
    let log = routes.clone();
    vcx.update(|_, cx| {
        cx.subscribe(&host, move |_, e: &SettingsEvent, _| {
            let SettingsEvent::Navigate(route) = e;
            log.borrow_mut().push(route.clone());
        })
        .detach();
    });
    vcx.run_until_parked();
    (vcx, routes)
}

/// Package 8c (Shell.tsx `settingsActivity`): while this device is in a call, Settings offers Return to call, which goes to
/// the call's channel; out of a call it is not there.
#[gpui::test]
fn settings_offer_return_to_call(cx: &mut TestAppContext) {
    use dilla_desktop::router::Route;
    cx.update(dilla_desktop::install);
    let (vcx, _) = open_settings(cx, Arc::new(AppState::default()));
    assert!(vcx.debug_bounds("settings-return-call").is_none());
    let (vcx, routes) = open_settings(cx, state(3));
    let back = vcx
        .debug_bounds("settings-return-call")
        .expect("Return to call")
        .center();
    vcx.simulate_click(back, gpui::Modifiers::none());
    vcx.run_until_parked();
    assert_eq!(
        routes.borrow().last(),
        Some(&Route::Channel {
            community: COMMUNITY,
            channel: Some(VOICE)
        })
    );
}

/// Package 8b: the person's volume reads as call/volume.ts does (100 when absent or not a volume), and the rooms a move
/// can go to are the server's other voice channels, the AFK channel and the current one left out, with their head count.
#[test]
fn the_menu_reads_volumes_and_rooms_as_the_web() {
    use dilla_client::features::voice_prefs::values::VoiceValues;
    let bob = UserId([1; 16]);
    let mut v = VoiceValues::default();
    assert_eq!(voice::menu::volume_of(&v, bob, false), 100);
    v.volumes.insert(bob.hex(), 50);
    v.stream_volumes.insert(bob.hex(), 0);
    assert_eq!(voice::menu::volume_of(&v, bob, false), 50);
    assert_eq!(voice::menu::volume_of(&v, bob, true), 0);
    v.volumes.insert(bob.hex(), 55);
    assert_eq!(voice::menu::volume_of(&v, bob, false), 100, "not a volume");

    let mut s = (*state(3)).clone();
    let mut up = channel();
    up.id = ChannelId([10; 16]);
    up.name = "Upstairs".into();
    let mut text = channel();
    text.id = ChannelId([11; 16]);
    text.kind = 0;
    s.channels.push(up);
    s.channels.push(text);
    let rooms = voice::menu::rooms(&s, &channel());
    assert_eq!(rooms, vec![(ChannelId([10; 16]), "Upstairs".to_owned(), 0)]);
}

const DM: ChannelId = ChannelId([7; 16]);

/// The DM between the viewer (user 0, "Alice") and Bob (user 1), as the shell hands it to the voice area.
fn dm_channel() -> Channel {
    Channel {
        id: DM,
        community_id: CommunityId([0; 16]),
        kind: 0,
        name: "Bob".into(),
        ..channel()
    }
}

/// Alice and Bob in their DM's call; Bob shares his screen.
fn dm_state() -> Arc<AppState> {
    let mut s = (*state(2)).clone();
    s.voice.place(
        DM,
        Some(DM.0),
        vec![
            VoiceParticipant {
                user_id: UserId([0; 16]),
                device_id: DeviceId([100; 16]),
                flags: flags::IN_CALL,
                own: true,
            },
            VoiceParticipant {
                user_id: UserId([1; 16]),
                device_id: DeviceId([101; 16]),
                flags: flags::IN_CALL | flags::SCREEN,
                own: false,
            },
        ],
    );
    s.voice.speaking.clear();
    s.voice.call = OwnCall {
        phase: CallPhase::In,
        channel: Some(DM),
        community: None,
        call_id: Some(DM.0),
        ..OwnCall::default()
    };
    Arc::new(s)
}

/// A debug selector built at run time (GPUI looks selectors up by `&'static str`); a test leaks a few bytes.
fn selector(s: String) -> &'static str {
    Box::leak(s.into_boxed_str())
}

/// A DM conversation's column: the call box over the log.
struct DmHost;

impl Render for DmHost {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let dm = dm_channel();
        div().size_full().flex().flex_col().child(
            div()
                .debug_selector(|| "dm-main".to_owned())
                .flex_1()
                .min_h_0()
                .flex()
                .flex_col()
                .children(voice::dm_call_box(&dm, window, cx))
                .child(div().flex_1().min_h_0()),
        )
    }
}

/// e2e/voice/stage.spec.ts "a DM call box keeps compact tiles when the other person shares their screen": the DM call
/// box is not the channel stage. With Bob's shared screen showing (its first frame has come), the box keeps its compact
/// grid: no stage, no focused tile and no full-screen button; the share's picture is more than a line, the share leads,
/// and every card is at most 40 % of the conversation's height.
#[gpui::test]
fn a_dm_call_box_keeps_compact_tiles_when_the_other_person_shares_their_screen(
    cx: &mut TestAppContext,
) {
    cx.update(dilla_desktop::install);
    let (_, cx) = cx.add_window_view(|_, _| DmHost);
    let frame = image::RgbaImage::from_pixel(1920, 1080, image::Rgba([40, 90, 160, 255]));
    let picture = Arc::new(gpui::RenderImage::new(vec![image::Frame::new(frame)]));
    cx.update(|window, cx| {
        let w = voice::wid(window);
        voice::sync(w, dm_state(), None, cx);
        voice::ui_mut(w, cx).put_test_frame(
            dilla_media::StreamKey {
                device: DeviceId([101; 16]).hex(),
                kind: dilla_media::ShareKind::Screen,
            },
            picture,
        );
        window.refresh();
    });
    // The first frame measures the grid; the next lays its columns out.
    cx.run_until_parked();
    cx.update(|window, _| window.refresh());
    cx.run_until_parked();

    let main = cx.debug_bounds("dm-main").expect("the conversation");
    let main_h = f32::from(main.size.height);
    assert!(main_h > 0.0);
    assert!(cx.debug_bounds("dm-call-box").is_some(), "the box shows");
    assert!(
        cx.debug_bounds("voice-stage-root").is_none(),
        "the DM call is never the channel stage (no stage mode, no focus, no full screen)"
    );
    let own = DeviceId([100; 16]).hex();
    let bob = DeviceId([101; 16]).hex();
    let screen = cx
        .debug_bounds(selector(format!("dm-tile-{bob}-screen")))
        .expect("Bob's screen's card");
    let cards = [
        screen,
        cx.debug_bounds(selector(format!("dm-tile-{own}")))
            .expect("the own card"),
        cx.debug_bounds(selector(format!("dm-tile-{bob}")))
            .expect("Bob's card"),
    ];
    for card in cards {
        let h = f32::from(card.size.height);
        assert!(h > 20.0, "a card is more than a line: {h}");
        assert!(
            h <= main_h * 0.4,
            "a DM box card's height {h} against 40 % of the pane ({main_h})"
        );
    }
    let picture = cx
        .debug_bounds(selector(format!("dm-tile-{bob}-screen-media")))
        .expect("the shared screen's picture");
    assert!(
        f32::from(picture.size.height) > 20.0,
        "the shared screen's picture height: {:?}",
        picture.size
    );
    let ordered = cards.iter().all(|c| c.origin.y >= screen.origin.y)
        && cards[1..]
            .iter()
            .all(|c| c.origin.y > screen.origin.y || c.origin.x > screen.origin.x);
    assert!(ordered, "the share leads the grid: {cards:?}");
    let tiles = area(cx, |ui| voice::dm_tiles(&ui.state, DM));
    assert_eq!(tiles.len(), 3);
    assert!(tiles[0].screen && !tiles[1].screen && !tiles[2].screen);
    assert_eq!(
        area(cx, |ui| ui.focused.clone()),
        None,
        "nothing is focused"
    );
}

/// Wave 2 review, lane G minor: in a narrow conversation (360x700, one column of cards) the DM call box stays compact:
/// the box and each card are at most 40 % of the conversation's height.
#[gpui::test]
fn a_dm_call_box_stays_compact_in_a_narrow_conversation(cx: &mut TestAppContext) {
    cx.update(dilla_desktop::install);
    let (_, cx) = cx.add_window_view(|_, _| DmHost);
    cx.simulate_resize(gpui::size(gpui::px(360.0), gpui::px(700.0)));
    cx.update(|window, cx| {
        voice::sync(voice::wid(window), dm_state(), None, cx);
        window.refresh();
    });
    for _ in 0..3 {
        cx.run_until_parked();
        cx.update(|window, _| window.refresh());
    }
    let main_h = f32::from(
        cx.debug_bounds("dm-main")
            .expect("the conversation")
            .size
            .height,
    );
    let boxed = cx.debug_bounds("dm-call-box").expect("the box shows");
    assert!(
        f32::from(boxed.size.height) <= main_h * 0.4 + 0.5,
        "the box {:?} against 40 % of {main_h}",
        boxed.size
    );
    let own = DeviceId([100; 16]).hex();
    let card = cx
        .debug_bounds(selector(format!("dm-tile-{own}")))
        .expect("the own card");
    assert!(f32::from(card.size.height) <= main_h * 0.4);
    // One column: the card takes the grid's width.
    let bob = cx
        .debug_bounds(selector(format!("dm-tile-{}", DeviceId([101; 16]).hex())))
        .expect("Bob's card");
    assert!(
        bob.origin.y > card.origin.y,
        "one column at 360px: {card:?} {bob:?}"
    );
}

/// Wave 2 review I-G2 (CallStage.tsx's `<details>`): the DM box's "call code" is a disclosure. A click opens it on the
/// grouped safety-code digits (six groups of five), a second click closes it; Escape closes it too.
#[gpui::test]
fn the_dm_call_code_opens_on_its_grouped_digits(cx: &mut TestAppContext) {
    cx.update(dilla_desktop::install);
    let (_, cx) = cx.add_window_view(|_, _| DmHost);
    let mut s = (*dm_state()).clone();
    s.voice.call.code = Some("123451234512345123451234512345".into());
    let s = Arc::new(s);
    cx.update(|window, cx| {
        voice::sync(voice::wid(window), s.clone(), None, cx);
        window.refresh();
    });
    cx.run_until_parked();
    let summary = cx
        .debug_bounds("dm-call-code")
        .expect("the call code's summary");
    assert!(
        cx.debug_bounds("dm-call-code-digits").is_none(),
        "closed at first"
    );
    cx.simulate_click(summary.center(), gpui::Modifiers::none());
    cx.update(|window, _| window.refresh());
    cx.run_until_parked();
    assert!(cx.debug_bounds("dm-call-code-open").is_some());
    let digits = cx
        .debug_bounds("dm-call-code-digits")
        .expect("the digits show");
    assert!(digits.origin.y >= summary.origin.y + summary.size.height);
    assert_eq!(
        voice::group_code("123451234512345123451234512345"),
        "12345 12345 12345 12345 12345 12345"
    );
    assert_eq!(
        voice::group_code("12345 6"),
        "12345 6",
        "not 30 digits: as given"
    );
    // A second click closes it.
    cx.simulate_click(summary.center(), gpui::Modifiers::none());
    cx.update(|window, _| window.refresh());
    cx.run_until_parked();
    assert!(cx.debug_bounds("dm-call-code-digits").is_none());
    // Opened again, Escape closes it.
    cx.simulate_click(summary.center(), gpui::Modifiers::none());
    cx.update(|window, _| window.refresh());
    cx.run_until_parked();
    assert!(cx.debug_bounds("dm-call-code-digits").is_some());
    cx.simulate_keystrokes("escape");
    cx.update(|window, _| window.refresh());
    cx.run_until_parked();
    assert!(cx.debug_bounds("dm-call-code-digits").is_none());
    assert!(cx.debug_bounds("dm-call-code").is_some());
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

/// Package 9 (web ptt.ts refuses `getModifierState('AltGraph')`): on Linux AltGr (xkb's Level3) with the key never opens
/// the mic, though gpui reports no modifier for it; the bare key still does once AltGr is let go.
#[cfg(target_os = "linux")]
#[gpui::test]
fn altgr_with_the_ptt_key_does_not_open_the_mic(cx: &mut TestAppContext) {
    let (vcx, w, _) = field_host(cx);
    // The keyboard's state as xkb reports AltGr held: the keystroke itself looks bare.
    gpui_linux::set_level3_shift_active(true);
    key_down(vcx, "insert");
    let opened = vcx.update(|_, cx| voice::ptt_sending(w, cx));
    gpui_linux::set_level3_shift_active(false);
    assert!(!opened, "AltGr with the key: not push to talk");
    key_down(vcx, "insert");
    assert!(
        vcx.update(|_, cx| voice::ptt_sending(w, cx)),
        "the bare key opens the mic"
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

/// A call's media made current in the process's router for a test, ended when it drops (review M M6: an assert that
/// fails while it is live would otherwise leave it current for every later test of the binary).
struct LiveMedia {
    router: std::sync::Arc<dilla_media::transform::Router>,
    media: std::sync::Arc<dilla_media::transform::CallMedia>,
}

impl Drop for LiveMedia {
    fn drop(&mut self) {
        self.router.end(&self.media);
    }
}

/// The race the slot closes, forced: a call live in this process (another test's window) holds the next real-instance
/// test, instead of that test's media starting into `E_CALL_BUSY`; the wait ends once the live call's media ends.
///
/// Review M M7: it installs the process's frame hook and starts call media, so it is gated as the call tests are (the
/// desktop-gpui job runs it with `--ignored` on Linux).
#[test]
#[ignore = "starts call media (the dilla frame hook): run with the call tests (--ignored)"]
fn a_live_call_in_the_process_holds_the_next_call_test_until_its_media_ends() {
    // The slot, once an earlier test's media is gone (a window's media session can outlive its test).
    let _slot = media_slot();
    let router = dilla_media::transform::Router::get().expect("the dilla frame hook installs");
    let live = dilla_media::transform::CallMedia::new();
    router.begin(live.clone()).expect("no call is live yet");
    let guard = LiveMedia {
        router: router.clone(),
        media: live.clone(),
    };
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
    drop(guard);
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
    // Bob is in the call too (signalling only: the process's one media slot is alice's), so alice is not alone and
    // her leave is her voice Remove, which bob commits.
    w.rt.block_on(w.bob.voice().join(w.voice))
        .unwrap()
        .expect("bob joins");
    let alice_device = DeviceId(w.alice.state().borrow().account.device_id.unwrap().0);
    w.rt.block_on(until(&w.bob, "bob keys alice's leaf", |s| {
        s.voice.call.roster.iter().any(|r| r.1 == alice_device)
    }));
    // Review L I3 / HL I2: the quit's answer is the leaves' outcome, not only that they answered: `leave` answers Ok
    // only once the instance acknowledged the voice Remove (`calls.leave`), so a Remove that fails fails this.
    assert!(
        cx.update(voice::on_quit),
        "the call's leave answered Ok within the quit's wait"
    );
    assert_eq!(phase_of(&w.alice), CallPhase::Idle);
    assert!(
        w.alice.voice().media().borrow().is_none(),
        "the media link is cut"
    );
    // The Remove reached the instance: bob, the call's other member, commits it and alice's leaf leaves his roster.
    w.rt.block_on(until(
        &w.bob,
        "alice's device leaves bob's call roster",
        |s| {
            s.voice.call.phase == CallPhase::In
                && !s.voice.call.roster.iter().any(|r| r.1 == alice_device)
        },
    ));
}

/// Re-review N1: removing an instance from the manager while its window is in a call leaves the call first, as Forget
/// and Sign out do: the others see the device go at once (its voice Remove), not after the instance notices a silent
/// device.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn removing_an_instance_in_a_call_leaves_the_call(cx: &mut TestAppContext) {
    use dilla_desktop::{AppView, Launch, windows};
    let tag = "t-remove-call";
    let media = media_slot();
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let (_host, alice, bob, voice_channel) = rt.block_on(async {
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
        let b = call_world::start(&bob).await;
        call_world::connected(&b).await;
        b.voice().start();
        (host, alice, b, voice)
    });
    let root = alice
        .cfg
        .data_dir
        .parent()
        .and_then(std::path::Path::parent)
        .unwrap()
        .to_path_buf();
    let launch = Launch::live(Some(root), alice.kc.clone());
    cx.update(dilla_desktop::install);
    cx.executor().allow_parking();
    cx.update(|cx| windows::open_instance(cx, &launch, alice.cfg.clone(), None, false))
        .expect("the instance window opens");
    let id = alice.cfg.id;
    // The shell shows on alice's client (the compact start window hands it to a workspace window).
    let deadline = std::time::Instant::now() + CALL_WAIT;
    let (client, window) = loop {
        cx.run_until_parked();
        let shell = cx
            .windows()
            .iter()
            .filter_map(|w| w.downcast::<AppView>())
            .find(|w| {
                w.read_with(cx, |v, _| v.screen_name() == "shell")
                    .unwrap_or(false)
            });
        if let (Some(w), Some(c)) = (shell, cx.update(|cx| windows::running_client(cx, id))) {
            break (c, w.window_id());
        }
        assert!(std::time::Instant::now() < deadline, "the shell showed");
        std::thread::sleep(std::time::Duration::from_millis(50));
    };
    pump(cx, &rt, &client, "the voice channel is listed", |s| {
        s.channel(voice_channel).is_some()
    });
    cx.update(|cx| voice::join(voice_channel, window, cx));
    pump(cx, &rt, &client, "alice is in the call", |s| {
        s.voice.call.phase == CallPhase::In
    });
    // Bob is in the call too (signalling only: the process's one media slot is alice's), so alice's leave is her voice
    // Remove, which bob commits.
    rt.block_on(bob.voice().join(voice_channel))
        .unwrap()
        .expect("bob joins");
    let alice_device = DeviceId(client.state().borrow().account.device_id.unwrap().0);
    rt.block_on(until(&bob, "bob keys alice's leaf", |s| {
        s.voice.call.roster.iter().any(|r| r.1 == alice_device)
    }));
    drop(client);

    let manager = cx
        .update(|cx| windows::open_manager(cx, &launch, false))
        .expect("the manager opens");
    let index = manager
        .read_with(cx, |v, _| v.entries().iter().position(|e| e.id == id))
        .unwrap()
        .expect("the instance is listed");
    manager
        .update(cx, |view, _, cx| view.ask_remove(index, cx))
        .unwrap();
    manager
        .update(cx, |view, window, cx| view.confirm(window, cx))
        .unwrap();
    // At once: the leave is the Remove's own, not the instance's eventual timeout of a silent device.
    let left = std::time::Instant::now() + std::time::Duration::from_secs(8);
    let mut moved = std::time::Instant::now();
    loop {
        cx.run_until_parked();
        let gone = !bob
            .state()
            .borrow()
            .voice
            .call
            .roster
            .iter()
            .any(|r| r.1 == alice_device);
        if gone {
            break;
        }
        assert!(
            std::time::Instant::now() < left,
            "the removed device left bob's call roster at once"
        );
        std::thread::sleep(std::time::Duration::from_millis(30));
        cx.executor().advance_clock(moved.elapsed());
        moved = std::time::Instant::now();
    }
    // The removal still finishes: the instance is gone from the list.
    let done = std::time::Instant::now() + CALL_WAIT;
    loop {
        cx.run_until_parked();
        let listed = manager
            .read_with(cx, |v, _| v.entries().iter().any(|e| e.id == id))
            .unwrap();
        if !listed {
            break;
        }
        assert!(std::time::Instant::now() < done, "the removal finished");
        std::thread::sleep(std::time::Duration::from_millis(30));
        cx.executor().advance_clock(moved.elapsed());
        moved = std::time::Instant::now();
    }
    assert_eq!(
        manager
            .read_with(cx, |v, _| v.error().map(str::to_owned))
            .unwrap(),
        None
    );
    drop(bob);
    drop(media);
}

/// Review L I3: a leave that answers an error, or whose answer is dropped, is no successful quit.
#[test]
fn a_failed_leave_fails_the_quit_s_wait() {
    let (tx, failed) = answer_pair();
    tx.send(Err(dilla_client::ClientError::Network("down".into())))
        .unwrap();
    assert!(!voice::wait_answers(
        vec![failed],
        std::time::Duration::from_secs(5)
    ));
    let (tx, closed) = answer_pair();
    drop(tx);
    assert!(!voice::wait_answers(
        vec![closed],
        std::time::Duration::from_secs(5)
    ));
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
    let (tx2, answered2) = answer_pair();
    tx2.send(Ok(())).unwrap();
    assert!(voice::wait_answers(
        vec![answered, answered2],
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

/// A machine with no audio device at all: no microphone, no speaker.
struct NoAudio;

impl dilla_media::devices::AudioHost for NoAudio {
    fn list(&self) -> dilla_media::devices::DeviceList {
        dilla_media::devices::DeviceList::default()
    }

    fn open_input(
        &self,
        _: Option<&dilla_media::devices::DeviceId>,
        _: dilla_media::devices::InputSink,
        _: dilla_media::devices::OnLost,
    ) -> Result<Box<dyn dilla_media::devices::Running>, dilla_media::MediaError> {
        Err(dilla_media::MediaError::Audio("no microphone".into()))
    }

    fn open_output(
        &self,
        _: Option<&dilla_media::devices::DeviceId>,
        _: dilla_media::devices::OutputSource,
        _: dilla_media::devices::OnLost,
    ) -> Result<Box<dyn dilla_media::devices::Running>, dilla_media::MediaError> {
        Err(dilla_media::MediaError::Audio("no speaker".into()))
    }
}

/// Package 8a (beta.5 on a machine with no sound devices): the join goes through, listening, with beta.5's one notice
/// (voice.micMissing, "No microphone found. You can listen."), and stays: before, the listen-only retry failed on the
/// missing speaker too, said "Could not join voice" (MediaFailed) and left.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn no_output_device_joins_with_a_notice(cx: &mut TestAppContext) {
    let w = call_world("p8a-no-speaker");
    cx.update(dilla_desktop::install);
    cx.update(|cx| voice::use_audio_host(Arc::new(NoAudio), cx));
    let a = window_for(cx, &w.alice);
    let wa = a.window_id();
    cx.update(|cx| voice::join(w.voice, wa, cx));
    cx.run_until_parked();
    w.rt.block_on(until(&w.alice, "alice is in the call", |s| {
        s.voice.call.phase == CallPhase::In
    }));
    w.rt.block_on(until(
        &w.alice,
        "alice is told there is no microphone",
        |s| s.voice.notice == Some(dilla_client::state::voice::VoiceNotice::MicLost),
    ));
    // The media had its time to fail: the call stays, with the microphone notice and no other.
    std::thread::sleep(std::time::Duration::from_secs(3));
    let s = w.alice.state().borrow().clone();
    assert_eq!(s.voice.call.phase, CallPhase::In, "the call stays");
    assert_eq!(
        s.voice.notice,
        Some(dilla_client::state::voice::VoiceNotice::MicLost)
    );
    // Bob joins and sees alice in the call.
    w.rt.block_on(w.bob.voice().join(w.voice))
        .unwrap()
        .expect("bob joins");
    let alice_device = s.account.device_id.unwrap();
    w.rt.block_on(until(&w.bob, "bob lists alice in the call", |s| {
        s.voice
            .participants(w.voice)
            .iter()
            .any(|p| p.device_id == alice_device)
    }));
}

/// A window that draws `channel`'s stage from its client's state (the live tests' voice channel is the world's).
struct LiveStage {
    client: Rc<dilla_client::Client>,
    channel: ChannelId,
}

impl Render for LiveStage {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let state = self.client.state().borrow().clone();
        let ch = state.channel(self.channel).cloned();
        div()
            .size_full()
            .flex()
            .children(ch.and_then(|ch| voice::stage(&ch, window, cx)))
    }
}

/// Opens a window on `client`'s stage of `channel`, its voice area synced.
fn live_stage(
    cx: &mut TestAppContext,
    client: &Rc<dilla_client::Client>,
    channel: ChannelId,
) -> VisualTestContext {
    let w = cx.add_window(|_, _| LiveStage {
        client: client.clone(),
        channel,
    });
    let id = w.window_id();
    let state = client.state().borrow().clone();
    cx.update(|cx| voice::sync(id, state, Some(client), cx));
    VisualTestContext::from_window(w.into(), cx)
}

/// Syncs `vcx`'s voice area with `client`'s newest state and draws.
fn resync(vcx: &mut VisualTestContext, client: &Rc<dilla_client::Client>) {
    let state = client.state().borrow().clone();
    vcx.update(|window, cx| {
        voice::sync(voice::wid(window), state, Some(client), cx);
        window.refresh();
    });
    vcx.run_until_parked();
}

fn click(vcx: &mut VisualTestContext, selector: &'static str) {
    let at = vcx
        .debug_bounds(selector)
        .unwrap_or_else(|| panic!("{selector} is drawn"))
        .center();
    vcx.simulate_click(at, gpui::Modifiers::none());
    vcx.run_until_parked();
}

/// Package 8b (VoiceView.tsx `personMenu`, call/volume.ts): in a live call, the person menu on bob's tile offers his
/// volume; Mute for me sets it to 0 in this device's voice settings (which the call plays at), and again puts back what it
/// was; the menu stays open meanwhile (an on/off item).
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn the_tile_menu_sets_volume_and_mute_for_me(cx: &mut TestAppContext) {
    let w = call_world("p8b-menu");
    cx.update(dilla_desktop::install);
    let mut vcx = live_stage(cx, &w.alice, w.voice);
    let wa = vcx.update(|window, _| voice::wid(window));
    vcx.update(|_, cx| voice::join(w.voice, wa, cx));
    vcx.run_until_parked();
    w.rt.block_on(until(&w.alice, "alice is in the call", |s| {
        s.voice.call.phase == CallPhase::In
    }));
    // Bob joins with his media (a `call-peer`): a device is in the call's roster once its media is in the room.
    let bob_media = bob_with_media(&w);
    let bob = w.bob.state().borrow().account.user_id.unwrap();
    w.rt.block_on(until(&w.alice, "alice sees bob in the call", |s| {
        s.voice
            .participants(w.voice)
            .iter()
            .any(|p| p.user_id == bob)
    }));
    resync(&mut vcx, &w.alice);
    let key = vcx.update(|window, cx| {
        let ui = voice::ui_of(voice::wid(window), cx).unwrap();
        voice::tiles(&ui.state, w.voice)
            .into_iter()
            .find(|k| k.user == bob && !k.screen)
            .expect("bob's tile")
    });
    let at = vcx
        .update(|window, cx| {
            voice::ui_of(voice::wid(window), cx)
                .and_then(|u| u.menu.bounds.borrow().get(&key).copied())
        })
        .expect("bob's tile was laid out")
        .center();
    vcx.simulate_event(gpui::MouseDownEvent {
        button: gpui::MouseButton::Right,
        position: at,
        modifiers: gpui::Modifiers::none(),
        click_count: 1,
        first_mouse: false,
    });
    vcx.run_until_parked();
    let volume =
        |c: &dilla_client::Client| c.voice_prefs().values().volumes.get(&bob.hex()).copied();
    assert_eq!(volume(&w.alice), None, "100 %: no entry");
    click(&mut vcx, "menu-item-mute-for-me");
    assert_eq!(volume(&w.alice), Some(0), "muted for alice");
    assert!(
        vcx.update(|window, cx| voice::menu::is_open(voice::wid(window), cx)),
        "an on/off item keeps the menu open"
    );
    click(&mut vcx, "menu-item-mute-for-me");
    assert_eq!(volume(&w.alice), None, "back to 100 %");
    // A volume set in the menu is what the call plays bob at (the session's gains read the same values).
    vcx.update(|_, cx| voice::menu::set_volume(wa, bob, false, 150, cx));
    let s = w.alice.state().borrow().clone();
    let gains = voice::session::gains(&w.alice.voice_prefs().values(), &s);
    let bob_device = s
        .voice
        .participants(w.voice)
        .iter()
        .find(|p| p.user_id == bob)
        .unwrap()
        .device_id;
    assert_eq!(gains[&(bob_device.hex(), false)], 1.5);
    bob_media.quit();
}

/// Bob joins `w`'s call with his media in a `call-peer` process of its own (the frame hook is process-wide).
fn bob_with_media(w: &CallWorld) -> call_world::MediaPeer {
    w.rt.block_on(async {
        tokio::task::spawn_blocking(call_world::MediaPeer::build)
            .await
            .unwrap();
        w.bob
            .voice()
            .join(w.voice)
            .await
            .unwrap()
            .expect("bob joins");
        let link = w
            .bob
            .voice()
            .media()
            .borrow()
            .clone()
            .expect("a media link");
        let url = w._host.rtc_url(&link.url);
        let token = link.token.to_string();
        let first = link.epochs.borrow().clone().expect("the join epoch");
        tokio::task::spawn_blocking(move || {
            let mut m = call_world::MediaPeer::start(&url, &token, 660);
            m.epoch(&first);
            m.next("connected", std::time::Duration::from_secs(30));
            m
        })
        .await
        .unwrap()
    })
}

/// Package 8b (Shell.tsx `onMove`): your own menu's Move to joins the other room, which moves this device there, and says
/// "You moved to {room}".
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn move_to_moves_the_member(cx: &mut TestAppContext) {
    let media = media_slot();
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let tag = "p8b-move";
    let (_host, alice, voice_a, voice_b) = rt.block_on(async {
        let host = tokio::task::spawn_blocking(move || call_world::start_host_reachable(tag))
            .await
            .unwrap();
        let invite = host.invites.last().unwrap().clone();
        let alice = call_world::sign_up(&host, &invite, "alice", tag).await;
        let (host, alice, a, b) = tokio::task::spawn_blocking(move || {
            let (c, a) = call_world::voice_community(&host, &alice, &[]);
            let (b, _, _) = call_world::ds_of(&host, &alice)
                .create_channel(&c, 1, "upstairs")
                .unwrap();
            call_world::clear_session(&alice);
            (host, alice, ChannelId(a), ChannelId(b))
        })
        .await
        .unwrap();
        let client = call_world::start(&alice).await;
        call_world::connected(&client).await;
        client.voice().start();
        until(&client, "both voice channels are listed", |s| {
            s.channel(a).is_some() && s.channel(b).is_some()
        })
        .await;
        (host, client, a, b)
    });
    let alice = Rc::new(alice);
    cx.update(dilla_desktop::install);
    let mut vcx = live_stage(cx, &alice, voice_a);
    let wa = vcx.update(|window, _| voice::wid(window));
    vcx.update(|_, cx| voice::join(voice_a, wa, cx));
    vcx.run_until_parked();
    rt.block_on(until(&alice, "alice is in the first room", |s| {
        s.voice.call.phase == CallPhase::In
            && s.voice.call.channel == Some(voice_a)
            && s.voice.participants(voice_a).iter().any(|p| p.own)
    }));
    resync(&mut vcx, &alice);
    let own = vcx.update(|window, cx| {
        let ui = voice::ui_of(voice::wid(window), cx).unwrap();
        voice::tiles(&ui.state, voice_a)
            .into_iter()
            .next()
            .expect("alice's own tile")
    });
    vcx.update(|window, cx| {
        voice::menu::open_from_key(voice::wid(window), own, cx);
        window.refresh();
    });
    vcx.run_until_parked();
    let item: &'static str = format!("menu-item-move:{}", voice_b.hex()).leak();
    click(&mut vcx, item);
    // The move's join runs on the UI's executor (the handover), so it is pumped with the clients.
    pump(cx, &rt, &alice, "alice moved to upstairs", |s| {
        s.voice.call.phase == CallPhase::In && s.voice.call.channel == Some(voice_b)
    });
    let s = rt.block_on(until(&alice, "alice is listed upstairs", |s| {
        s.voice.participants(voice_b).iter().any(|p| p.own)
    }));
    assert!(
        !s.voice.participants(voice_a).iter().any(|p| p.own),
        "and no longer in the first room"
    );
    drop(vcx);
    drop(alice);
    drop(rt);
    drop(media);
}

/// Review L m1: one call per device, the newest join wins. A handover still waiting for another window's leave is dropped
/// when a newer join starts (in any window): else both would join once the leave answered.
#[gpui::test]
fn a_newer_join_drops_an_older_waiting_handover(cx: &mut TestAppContext) {
    let (_a, a) = handover_window(cx);
    let b = cx.add_window(|_, _| Host).window_id();
    cx.update(|cx| voice::sync(b, state(2), None, cx));
    let (older, newer) = (Rc::new(Did::default()), Rc::new(Did::default()));
    let (leave_tx, leave_rx) = answer_pair();
    let (_join_a_tx, join_a_rx) = answer_pair();
    let (start, undo) = start_and_undo(&older, join_a_rx);
    cx.update(|cx| voice::handover(a, vec![leave_rx], start, undo, cx));
    cx.run_until_parked();
    // B's join finds no live call (the other window's leave already published Idle) and starts at once.
    let (_join_b_tx, join_b_rx) = answer_pair();
    let (start, undo) = start_and_undo(&newer, join_b_rx);
    cx.update(|cx| voice::handover(b, Vec::new(), start, undo, cx));
    cx.run_until_parked();
    assert!(newer.started.get());
    leave_tx.send(Ok(())).unwrap();
    cx.executor()
        .advance_clock(std::time::Duration::from_millis(200));
    cx.run_until_parked();
    assert!(!older.started.get(), "the older handover starts no join");
    assert_eq!(toast_in(cx, a), None, "a dropped handover says nothing");
}

/// Review L m2: a join whose answer never comes is not polled for as long as the app runs.
#[gpui::test]
fn a_join_that_never_answers_is_not_polled_forever(cx: &mut TestAppContext) {
    let (_w, id) = handover_window(cx);
    let did = Rc::new(Did::default());
    let (join_tx, join_rx) = answer_pair();
    let (start, undo) = start_and_undo(&did, join_rx);
    cx.update(|cx| voice::handover(id, Vec::new(), start, undo, cx));
    cx.run_until_parked();
    assert!(did.started.get());
    cx.executor()
        .advance_clock(voice::JOIN_WAIT - std::time::Duration::from_secs(1));
    cx.run_until_parked();
    assert!(!join_tx.is_closed(), "still waiting within the bound");
    cx.executor()
        .advance_clock(std::time::Duration::from_secs(2));
    cx.run_until_parked();
    assert!(join_tx.is_closed(), "the poll gave up on the answer");
}

/// The toast a join that answers `answer` leaves in its window, after the handover.
fn toast_after_join(
    cx: &mut TestAppContext,
    answer: Result<(), dilla_client::ClientError>,
) -> Option<String> {
    let (_w, id) = handover_window(cx);
    let did = Rc::new(Did::default());
    let (join_tx, join_rx) = answer_pair();
    let (start, undo) = start_and_undo(&did, join_rx);
    cx.update(|cx| voice::handover(id, Vec::new(), start, undo, cx));
    cx.run_until_parked();
    join_tx.send(answer).unwrap();
    cx.executor()
        .advance_clock(std::time::Duration::from_millis(200));
    cx.run_until_parked();
    toast_in(cx, id)
}

fn sync_err(code: &str) -> Result<(), dilla_client::ClientError> {
    Err(dilla_client::ClientError::Sync(code.into()))
}

/// Review L I1 (shell-model.ts `joinFailureKey`): a join the person cancelled (a leave, or another join, while joining)
/// says nothing.
#[gpui::test]
fn a_cancelled_join_says_nothing(cx: &mut TestAppContext) {
    assert_eq!(toast_after_join(cx, sync_err("E_CANCELLED")), None);
}

/// Review L I1: a trust refusal says nothing in a toast; the stage's blocked notice says why.
#[gpui::test]
fn a_trust_refused_join_says_nothing(cx: &mut TestAppContext) {
    assert_eq!(toast_after_join(cx, sync_err("E_UMK_CHANGED")), None);
}

/// Review L I1: a full call says so.
#[gpui::test]
fn a_full_call_says_it_is_full(cx: &mut TestAppContext) {
    assert_eq!(
        toast_after_join(cx, sync_err("E_CALL_FULL")).as_deref(),
        Some(dilla_desktop::strings::t("voice.full"))
    );
}

/// Review L I1: `joinFailureKey`, code by code (shell-model.test.ts).
#[test]
fn a_join_failure_maps_as_the_web_s() {
    use dilla_client::ClientError;
    let key = |e: ClientError| voice::join_failure_key(&e);
    let sync = |c: &str| ClientError::Sync(c.into());
    assert_eq!(key(sync("E_CALL_FULL")), Some("voice.full"));
    assert_eq!(key(sync("E_CALL_SHARERS_FULL")), Some("voice.sharersFull"));
    assert_eq!(key(sync("E_CALL_IN_OTHER_TAB")), Some("voice.otherTab"));
    for quiet in [
        "E_CANCELLED",
        "E_ABORTED",
        "E_UMK_CHANGED",
        "E_CREDENTIAL",
        "E_DEVICE_UNLISTED",
        "E_DEVICE_REVOKED",
        "E_TIER_MISMATCH",
        "E_BINDING",
    ] {
        assert_eq!(key(sync(quiet)), None, "{quiet}");
    }
    assert_eq!(key(sync("E_NETWORK")), Some("voice.failed"));
    assert_eq!(
        key(ClientError::Network("down".into())),
        Some("voice.failed")
    );
    assert_eq!(key(ClientError::UnknownChannel), Some("voice.failed"));
}

// ---- parity (S4, lane G): the app's frame of each fixture against the web client's (parity/references/linux/voice/) ----

fn parity_fixture(name: &str) -> std::path::PathBuf {
    std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/fixtures/voice")
        .join(format!("{name}.json"))
}

macro_rules! parity {
    ($($test:ident => $name:literal $(; $why:literal)?),* $(,)?) => {$(
        #[test]
        #[cfg_attr(not(target_os = "linux"), ignore = "references are Linux only")]
        $(#[cfg_attr(target_os = "linux", ignore = $why)])?  // off Linux the line above already ignores it
        fn $test() {
            let report = dilla_parity::screen::assert_screen(&parity_fixture($name))
                .unwrap_or_else(|e| panic!("voice/{} differs from its web reference: {e}", $name));
            println!("SCREEN voice/{}: {} text lines, chrome {:.4} %", $name, report.text_lines, report.chrome_pct);
        }
    )*};
}

parity! {
    parity_voice_stage => "stage",
    parity_voice_stage_b => "stage-b",
    parity_voice_dm_call => "dm-call",
    parity_voice_stage_focused => "stage-focused",
    parity_voice_person_menu => "person-menu",
}

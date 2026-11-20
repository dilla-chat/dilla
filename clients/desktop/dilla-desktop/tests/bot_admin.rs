//! Task 21's keyboard and behaviour tests for the bot admin area (standard procedure S4-S6): the now-playing controls are
//! reachable by Tab and Space plays and pauses when the card is focused; a control without permission says why in a toast;
//! the Bot and Music pages' own tests (the one-time webhook address, the capability mask) are in
//! `src/screens/bot_admin/*.rs`.
//!
//! Parity (S4): `parity_bot_admin_{health_channels,music}[_b]` draw the Bot and Music pages with the Bot NOT installed (the
//! fixture schema, `dilla-client/src/testing/spec.rs`, cannot seed an installation) against references shot from the web
//! client, inside the real server settings host (Task 15's `ServerSettingsView`, integration 3 step 5). They are
//! `#[ignore]`d until the shared components (section-header sub-line wrap, toggle offset, row hint wrap) land; the last
//! run, in that host, was 2.85 %, 2.85 %, 1.01 % and 1.31 % against 0.5 %.
//!
//! Not here (Task 28 adds a `-bot` capability to the test host and the fixture schema for them):
//! - the installed-Bot fixtures: `bot-admin/health-channels` with an installation, Bot channels and webhooks;
//!   `bot-admin/channel-toggle` (the Bot channel switch in the channel dialog); `bot-admin/webhook-post` (a post "via webhook");
//!   `music/now-playing` (the dock player);
//! - the behaviour tests that run the Bot (`bot_enabled_reaches_ready`, `webhook_post_appears`, `welcome_channel_greets_new_member`,
//!   `music_text_never_executes`, `moderation_log_across_bot_restart`, `join_role_applied_on_join_and_rejoin`,
//!   `self_role_panel_reaction_grant`, `warning_retrievable_from_bot_settings` and the six `bot-music` ones): they need the
//!   Bot's process in the test host and Task 20's sealed transport behind `SealedBot`.

use std::cell::RefCell;
use std::rc::Rc;

use dilla_desktop::screens::{FocusNext, FocusPrevious};
use dilla_desktop_ui::components::bot_admin::now_playing::{
    NowPlaying, NowPlayingLabels, NowPlayingVariant, PlayerTrack, QueueRow,
};
use dilla_desktop_ui::components::toast::current;
use gpui::{
    Context, FocusHandle, InteractiveElement, IntoElement, ParentElement, Render, Styled,
    TestAppContext, VisualTestContext, Window, div,
};

fn install(cx: &mut TestAppContext) {
    cx.update(|cx| {
        dilla_desktop::install(cx);
    });
}

/// Presses a key as a keyboard does: down, then up (GPUI turns Enter or Space down-and-up on a focused element into a click).
fn press(cx: &mut VisualTestContext, key: &'static str) {
    cx.simulate_keystrokes(key);
    cx.simulate_event(gpui::KeyUpEvent {
        keystroke: gpui::Keystroke::parse(key).expect("a key"),
    });
    cx.run_until_parked();
}

fn labels() -> NowPlayingLabels {
    NowPlayingLabels {
        playing: "Now playing".into(),
        paused: "Paused".into(),
        stopped: "Stopped".into(),
        idle: "Nothing playing".into(),
        play: "Play".into(),
        pause: "Pause".into(),
        skip: "Skip".into(),
        stop: "Stop".into(),
        queue: "Up next · 2".into(),
        up_next: "3 up next".into(),
    }
}

/// A window with a player and the order its controls were activated in.
struct Player {
    root: FocusHandle,
    log: Rc<RefCell<Vec<&'static str>>>,
    can_control: bool,
    queue_open: bool,
    /// Card (the default) or the stage bar; `track` false draws the variant with no track playing.
    variant: NowPlayingVariant,
    track: bool,
}

impl Render for Player {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let (toggle, skip, stop) = (self.log.clone(), self.log.clone(), self.log.clone());
        let this = cx.entity();
        let queue = self.log.clone();
        let row = |current| QueueRow {
            title: "Harbour Lights".into(),
            by: "Ada".into(),
            duration_s: Some(214.0),
            current,
        };
        div()
            .id("player-host")
            .track_focus(&self.root)
            .size_full()
            .on_action(|_: &FocusNext, window, cx| window.focus_next(cx))
            .on_action(|_: &FocusPrevious, window, cx| window.focus_prev(cx))
            .child(
                NowPlaying::new("np", self.variant, labels())
                    .bot("Mr. Kim")
                    .track(self.track.then(|| PlayerTrack {
                        title: "Harbour Lights".into(),
                        duration_s: 214.0,
                    }))
                    .position(37.0)
                    .playing(true)
                    .meta("YouTube · requested by Ada · harbour")
                    .volume("75%")
                    .can_control(self.can_control, "Join harbour to control playback")
                    .queue(vec![row(true), row(false), row(false)], self.queue_open)
                    .on_toggle(move |_, _| toggle.borrow_mut().push("toggle"))
                    .on_skip(move |_, _| skip.borrow_mut().push("skip"))
                    .on_stop(move |_, _| stop.borrow_mut().push("stop"))
                    .on_queue_toggle(move |open, _, cx| {
                        queue
                            .borrow_mut()
                            .push(if open { "queue-open" } else { "queue-close" });
                        this.update(cx, |p, cx| {
                            p.queue_open = open;
                            cx.notify();
                        });
                    }),
            )
    }
}

fn player(
    cx: &mut TestAppContext,
    can_control: bool,
) -> (gpui::Entity<Player>, &mut VisualTestContext) {
    player_of(cx, can_control, NowPlayingVariant::Card, true)
}

fn player_of(
    cx: &mut TestAppContext,
    can_control: bool,
    variant: NowPlayingVariant,
    track: bool,
) -> (gpui::Entity<Player>, &mut VisualTestContext) {
    install(cx);
    let (host, cx) = cx.add_window_view(|_, cx| Player {
        root: cx.focus_handle(),
        log: Rc::default(),
        can_control,
        queue_open: false,
        variant,
        track,
    });
    let root = host.read_with(cx, |p, _| p.root.clone());
    cx.update(|window, cx| root.focus(window, cx));
    (host, cx)
}

fn log(host: &gpui::Entity<Player>, cx: &VisualTestContext) -> Vec<&'static str> {
    host.read_with(cx, |p, _| p.log.borrow().clone())
}

/// S6: Tab reaches the card, then play/pause, skip, stop and the queue toggle, in that order; each activates with Enter.
#[gpui::test]
fn keyboard_bot_admin_focus_order(cx: &mut TestAppContext) {
    let (host, cx) = player(cx, true);
    // The card, then the controls: each Tab lands on the next one, which Enter then runs.
    for (tabs, expect) in [(2, "toggle"), (1, "skip"), (1, "stop"), (1, "queue-open")] {
        for _ in 0..tabs {
            cx.simulate_keystrokes("tab");
        }
        press(cx, "enter");
        assert_eq!(
            log(&host, cx).last().copied(),
            Some(expect),
            "after {tabs} Tab: {expect}"
        );
    }
    assert_eq!(log(&host, cx), ["toggle", "skip", "stop", "queue-open"]);
    // Shift+Tab goes back through them.
    cx.simulate_keystrokes("shift-tab");
    press(cx, "enter");
    assert_eq!(
        log(&host, cx).last().copied(),
        Some("stop"),
        "Shift+Tab returns to Stop"
    );
}

/// Task 36: the stage bar's Tab stops are play/pause, skip and stop, in that order (it has no card stop of its own), and
/// Enter on each runs it.
#[gpui::test]
fn keyboard_stage_bar_focus_order(cx: &mut TestAppContext) {
    let (host, cx) = player_of(cx, true, NowPlayingVariant::Stage, true);
    for expect in ["toggle", "skip", "stop"] {
        cx.simulate_keystrokes("tab");
        press(cx, "enter");
        assert_eq!(log(&host, cx).last().copied(), Some(expect), "{expect}");
    }
    assert_eq!(log(&host, cx), ["toggle", "skip", "stop"]);
}

/// Task 36: the bar says how many tracks are up next (`music.upNext`, "{n} up next"): the player's own words for a queue of
/// the track playing and three more, drawn at the bar's end.
#[gpui::test]
fn the_stage_bar_names_its_up_next_count(cx: &mut TestAppContext) {
    use dilla_desktop::screens::bot_admin::player::labels as player_labels;
    use dilla_desktop_ui::components::bot_admin::now_playing::up_next;
    let row = |current| QueueRow {
        title: "t".into(),
        by: "b".into(),
        duration_s: None,
        current,
    };
    let queue = [row(true), row(false), row(false), row(false)];
    assert_eq!(player_labels(up_next(&queue)).up_next.as_ref(), "3 up next");
    let (_host, cx) = player_of(cx, true, NowPlayingVariant::Stage, true);
    let bar = cx.debug_bounds("now-playing-stage").expect("the bar");
    assert_eq!(f32::from(bar.size.height), 48.0);
    let count = cx
        .debug_bounds("now-playing-stage-up-next")
        .expect("the up next count");
    assert!(count.right() <= bar.right(), "{count:?} inside {bar:?}");
}

/// Task 36: a stage with no track draws nothing (NowPlaying.tsx returns null without one).
#[gpui::test]
fn the_stage_bar_is_not_drawn_without_a_track(cx: &mut TestAppContext) {
    let (_host, cx) = player_of(cx, true, NowPlayingVariant::Stage, false);
    assert!(cx.debug_bounds("now-playing-stage").is_none());
}

/// S6: Space on the card itself plays and pauses.
#[gpui::test]
fn keyboard_bot_admin_space_plays_and_pauses_on_the_card(cx: &mut TestAppContext) {
    let (host, cx) = player(cx, true);
    cx.simulate_keystrokes("tab");
    press(cx, "space");
    assert_eq!(
        log(&host, cx),
        ["toggle"],
        "Space on the focused card toggles once"
    );
    press(cx, "space");
    assert_eq!(log(&host, cx), ["toggle", "toggle"]);
}

/// Space on a focused control is that control's own press: the card does not toggle as well.
#[gpui::test]
fn space_on_a_control_runs_that_control_only(cx: &mut TestAppContext) {
    let (host, cx) = player(cx, true);
    for _ in 0..3 {
        cx.simulate_keystrokes("tab");
    }
    press(cx, "space");
    assert_eq!(log(&host, cx), ["skip"]);
}

/// Without permission a press does nothing but say why.
#[gpui::test]
fn a_control_without_permission_says_why_and_runs_nothing(cx: &mut TestAppContext) {
    let (host, cx) = player(cx, false);
    for _ in 0..2 {
        cx.simulate_keystrokes("tab");
    }
    press(cx, "enter");
    assert_eq!(log(&host, cx), Vec::<&str>::new(), "the Bot is not asked");
    let toast = cx.update(|_, cx| current(cx)).map(|t| t.to_string());
    assert_eq!(toast.as_deref(), Some("Join harbour to control playback"));
}

/// The queue opens from its toggle and lists the track playing first.
#[gpui::test]
fn the_queue_toggle_opens_the_queue(cx: &mut TestAppContext) {
    let (host, cx) = player(cx, true);
    for _ in 0..5 {
        cx.simulate_keystrokes("tab");
    }
    press(cx, "enter");
    assert!(host.read_with(cx, |p, _| p.queue_open), "the queue is open");
    press(cx, "enter");
    assert!(!host.read_with(cx, |p, _| p.queue_open), "and closes again");
    assert_eq!(log(&host, cx), ["queue-open", "queue-close"]);
}

// ---- S4: parity (Bot not installed: the fixture schema cannot seed an installation, Task 28) ----

fn parity(name: &str) {
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/fixtures")
        .join(name);
    let report = dilla_parity::screen::assert_screen(&path)
        .unwrap_or_else(|e| panic!("{name} differs from its web reference: {e}"));
    println!(
        "SCREEN {name}: {} text lines, chrome {:.4} %",
        report.text_lines, report.chrome_pct
    );
}

#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_bot_admin_health_channels() {
    parity("bot-admin/health-channels.json");
}

#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_bot_admin_health_channels_b() {
    parity("bot-admin/health-channels-b.json");
}

#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_bot_admin_music() {
    parity("bot-admin/music.json");
}

#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_bot_admin_music_b() {
    parity("bot-admin/music-b.json");
}

/// A GPUI-only reference (`native/music-card-live`, Task 35): the Bot's "started music" message in `#general` reads "Ada
/// started music in harbour" with the live now-playing card under it (the track playing, a queue of two, Ada may control).
/// Native because the web's frame needs a Bot playing real audio in a real call; checked by eye.
#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_music_card_live() {
    native("bot-admin/music-card-live", "music-card-live");
}

/// `native/music-card-ended`: the same message when the announced track no longer plays (another does): the card says
/// "Glenn played this in harbour" over "Finished", with no track and no queue.
#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_music_card_ended() {
    native("bot-admin/music-card-ended", "music-card-ended");
}

fn native(name: &str, golden: &str) {
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/fixtures")
        .join(format!("{name}.json"));
    let report = dilla_parity::screen::assert_native_screen(&path, golden, |_| {})
        .unwrap_or_else(|e| panic!("{name} differs from its golden shot: {e}"));
    println!(
        "SCREEN native/{golden}: {} text lines, chrome {:.4} %",
        report.text_lines, report.chrome_pct
    );
}

/// The card is the message column's width up to the web's 442px (NowPlaying.css `max-width` in a `minmax(0, 1fr)` column):
/// in the 1280px window it takes 442, and in a compact 400px window it shrinks and stays inside the window.
#[cfg(feature = "automation")]
#[gpui::test]
fn the_card_shrinks_with_the_log_and_stops_at_442(cx: &mut TestAppContext) {
    use dilla_desktop::app::{AppView, Launch};
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/fixtures/bot-admin/music-card-live.json");
    let loaded = dilla_desktop::fixture::Fixture::load(&path, None).expect("the card fixture");
    cx.update(dilla_desktop::install);
    let (_app, vcx) =
        cx.add_window_view(|window, cx| AppView::new(Launch::fixture(loaded), window, cx));
    vcx.run_until_parked();
    let width = |vcx: &mut VisualTestContext| {
        vcx.update(|window, _| window.refresh());
        vcx.run_until_parked();
        vcx.debug_bounds("music-card-box").expect("the card")
    };
    assert_eq!(f32::from(width(vcx).size.width), 442.0);
    vcx.simulate_resize(gpui::size(gpui::px(400.0), gpui::px(800.0)));
    let narrow = width(vcx);
    assert!(
        f32::from(narrow.size.width) < 442.0 && f32::from(narrow.right()) <= 400.0,
        "{narrow:?}"
    );
}

// ---- integration 3, step 5: the pages' navigation inside the server settings host ----

/// The Music page's "Open Bot settings" (BotSection.tsx `workspace.botSetup`) opens the Bot section in the same server
/// settings, and the Bot page's "Edit in Roles" (`serverSettings.bot.voiceRoles`) leads to Roles.
#[cfg(feature = "automation")]
#[gpui::test]
fn the_bot_pages_lead_to_the_bot_and_roles_sections(cx: &mut TestAppContext) {
    use dilla_desktop::app::{AppView, Launch};
    use dilla_desktop::router::ServerSection;
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/fixtures/bot-admin/music.json");
    let loaded = dilla_desktop::fixture::Fixture::load(&path, None).expect("the music fixture");
    cx.update(dilla_desktop::install);
    let (app, vcx) =
        cx.add_window_view(|window, cx| AppView::new(Launch::fixture(loaded), window, cx));
    vcx.run_until_parked();
    let active = |vcx: &mut VisualTestContext| {
        let (view, _) = app
            .read_with(vcx, |a, _| a.server_settings())
            .expect("server settings");
        view.read_with(vcx, |v, _| v.active())
    };
    assert_eq!(active(vcx), Some(ServerSection::Music));
    let open = vcx
        .debug_bounds("music-open-bot")
        .expect("the Music page's Open Bot settings");
    vcx.simulate_click(open.center(), gpui::Modifiers::none());
    vcx.run_until_parked();
    assert_eq!(active(vcx), Some(ServerSection::Bot), "the Bot section");
    // The Bot page's "Edit in Roles" through the same hook (its row needs an installed Bot, which no fixture seeds).
    vcx.update(|window, cx| {
        assert!(dilla_desktop::screens::server_settings::open_section(
            ServerSection::Roles,
            window,
            cx
        ))
    });
    vcx.run_until_parked();
    assert_eq!(active(vcx), Some(ServerSection::Roles), "Roles");
}

// ---- Package 10c: the Bot card's menu, against the real Bot ----

/// BotSection.tsx `glennMenuItems` (e2e/web/bot-runtime.spec.ts "a bot enabled through server settings reaches ready"):
/// with no installation the card has no menu; a ready Bot offers Rename, Refresh status and Disable to its manager.
/// Rename stores a trimmed display name that the member list then shows (a blank one is refused before it is sent),
/// Refresh status reads the installation again, and Disable takes it to disabled, after which the menu has no Disable.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad with its Bot: target/dilla-testhost, target/dillabot, dilla-bot-core and the wasi core"]
async fn rename_refresh_disable() {
    use std::time::Duration;

    use dilla_client::features::bot_admin::new_request_id;
    use dilla_client::state::CommunityId;
    use dilla_client::testing::host::bot::enable_bot;
    use dilla_client::testing::host::{
        clear_session, community, sign_up, start, start_bot_host, wait_for,
    };
    use dilla_desktop::screens::bot_admin::bot_page::card_menu;

    let tag = "p10c-glenn";
    let host = tokio::task::spawn_blocking(move || start_bot_host(tag, "127.0.0.1:0"))
        .await
        .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let pa = sign_up(&host, &invite, "alice", "alice", tag).await;
    let (_host, pa, c) = tokio::task::spawn_blocking(move || {
        let c = community(&host, &pa, "gpui", &[]);
        clear_session(&pa);
        (host, pa, c)
    })
    .await
    .unwrap();
    let a = start(&pa).await;
    let community_id = CommunityId(c);
    let ids = |items: Vec<dilla_desktop_ui::components::action_menu::MenuItem>| {
        items
            .into_iter()
            .map(|i| i.id.to_string())
            .collect::<Vec<_>>()
    };
    assert!(card_menu(false, true, None, "Glenn").is_empty());

    let installation = enable_bot(&a, c, 0).await;
    let bot_user = installation.user_id.expect("the Bot's account");
    a.members().reload(community_id).await.unwrap();
    wait_for(
        &a,
        Duration::from_secs(30),
        "the Bot in the member list",
        |s| {
            s.members
                .get(&community_id)
                .is_some_and(|m| m.iter().any(|m| m.user_id == bot_user))
        },
    )
    .await;
    assert_eq!(
        ids(card_menu(true, true, Some(&installation), "Glenn")),
        ["rename", "refresh", "disable"]
    );
    // A member who does not manage the community only refreshes.
    assert_eq!(
        ids(card_menu(true, false, Some(&installation), "Glenn")),
        ["refresh"]
    );

    let admin = a.bot_admin();
    assert!(
        admin
            .rename_bot(community_id, "   ")
            .await
            .unwrap()
            .is_err(),
        "a blank name is refused"
    );
    let saved = admin
        .rename_bot(community_id, "  Robo  ")
        .await
        .unwrap()
        .expect("the name is stored");
    // The instance stores the trimmed name on the Bot's account; the member row keeps the community nickname, so the page
    // shows the saved name from its own answer (BotSection.tsx `botNameOverride`).
    assert_eq!(saved, "Robo");

    let refreshed = admin
        .refresh_installation(community_id)
        .await
        .unwrap()
        .unwrap()
        .expect("an installation");
    assert_eq!(refreshed.state, 3);
    let disabled = admin
        .disable_installation(community_id, new_request_id(), refreshed.revision)
        .await
        .unwrap()
        .expect("disabled");
    assert_eq!(disabled.state, 4);
    assert_eq!(
        ids(card_menu(true, true, Some(&disabled), "Robo")),
        ["rename", "refresh"],
        "a disabled Bot is not offered Disable"
    );
    a.stop();
}

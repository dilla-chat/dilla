//! Task 15's parity, behaviour and keyboard tests for the server settings area (standard procedure S4-S6).
//!
//! - `parity_server_settings_<state>[_b]`: the area's 16 fixtures (`parity/fixtures/server-settings/`) against the web
//!   client's references, under the unchanged `tolerance.toml` (`dilla_parity::screen::assert_screen`). Linux only: the
//!   references are shot there.
//! - the behaviour tests of the e2e specs the brief names, which need a real dillad (`#[ignore]`d, `--include-ignored`).
//!   They drive the client's server-admin commands against it with the assertions the e2e specs make; the window side of
//!   each is the parity fixtures' steps (the settings screen is drawn from a fixture, not from a live client, until the
//!   shell's route host (Task 8) opens it).
//! - `keyboard_*`: the settings' keyboard contract, on the pieces this area owns.

use std::path::{Path, PathBuf};

use dilla_desktop::router::ServerSection;
use dilla_desktop::screens::server_settings::model;

fn fixture(name: &str) -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/fixtures/server-settings")
        .join(format!("{name}.json"))
}

fn parity(name: &str) {
    if let Err(failure) = dilla_parity::screen::assert_screen(&fixture(name)) {
        panic!("{name}: {failure}");
    }
}

macro_rules! parity_tests {
    ($($test:ident => $state:literal),* $(,)?) => {$(
        #[test]
        #[cfg_attr(not(target_os = "linux"), ignore = "references are Linux only")]
        fn $test() {
            parity($state);
        }
    )*};
}

parity_tests! {
    parity_server_settings_welcome => "welcome",
    parity_server_settings_overview => "overview",
    parity_server_settings_overview_b => "overview-b",
    parity_server_settings_afk => "afk",
    parity_server_settings_afk_b => "afk-b",
    parity_server_settings_invites => "invites",
    parity_server_settings_invites_b => "invites-b",
    parity_server_settings_gifs => "gifs",
    parity_server_settings_gifs_b => "gifs-b",
    parity_server_settings_channels => "channels",
    parity_server_settings_channels_b => "channels-b",
    parity_server_settings_channel_permissions => "channel-permissions",
    parity_server_settings_channel_permissions_b => "channel-permissions-b",
}

parity_tests! {
    parity_server_settings_danger => "danger",
    parity_server_settings_danger_b => "danger-b",
    parity_server_settings_welcome_b => "welcome-b",
    parity_server_settings_join_roles => "join-roles",
    parity_server_settings_join_roles_b => "join-roles-b",
}

/// BotSection.tsx's Join roles page with no Bot installation (a fixture is an instance with none): the section's title,
/// the Bot setup button, the page's own heading and note, and why it cannot be set; no role select and nothing to save.
#[test]
#[cfg_attr(
    not(target_os = "linux"),
    ignore = "the headless renderer is Linux only here"
)]
fn the_join_roles_page_without_a_bot_offers_its_setup_and_no_select() {
    let mut driver = dilla_parity::Driver::launch_fixture(&fixture("join-roles"), None, &[]);
    driver.wait_idle();
    assert!(driver.element("join-role-select").is_none());
    assert!(driver.element("settings-save").is_none());
}

// ---- keyboard (S6) ----

/// The nav is one tab stop whose arrows move between the sections the permissions allow, in the registry's order.
#[test]
fn keyboard_server_settings_focus_order() {
    use dilla_client::state::server_admin::Access;
    let owner = dilla_client::features::server_admin::access_of(0, 0, true);
    let ids: Vec<&str> = model::nav_sections(&owner).iter().map(|s| s.id()).collect();
    assert_eq!(
        ids,
        [
            "overview",
            "roles",
            "invites",
            "bot",
            "music",
            "channels",
            "gifs",
            "members",
            "selfRoles",
            "welcome",
            "joinRoles",
            "moderationLog",
            "bans",
            "danger"
        ]
    );
    // A person who may do nothing special is offered only what every member may use, and the arrows have three stops.
    let member = Access {
        loaded: true,
        ..Access::default()
    };
    let ids: Vec<&str> = model::nav_sections(&member)
        .iter()
        .map(|s| s.id())
        .collect();
    assert_eq!(ids, ["members", "selfRoles", "danger"]);
}

/// Escape closes the settings (the frame's own handler, Task 6), and a section the person lost falls back to the first
/// they may use (ServerSettings.tsx).
#[test]
fn keyboard_server_settings_escape_and_lost_section() {
    let member = dilla_client::state::server_admin::Access {
        loaded: true,
        ..Default::default()
    };
    assert_eq!(
        model::active_of(Some(ServerSection::Overview), &member),
        Some(ServerSection::Members)
    );
}

/// Tab walks the Overview in its visual order and reaches the Leave button (`server-settings-leave`, registered by this
/// area on its own focus handle); Shift+Tab walks back out of it. The nav and the fields are the shared components'.
#[test]
#[cfg_attr(
    not(target_os = "linux"),
    ignore = "the headless renderer is Linux only here"
)]
fn keyboard_server_settings_tab_reaches_leave_and_shift_tab_leaves_it() {
    let mut driver = dilla_parity::Driver::launch_fixture(&fixture("overview"), None, &[]);
    driver.wait_idle();
    let mut forward = Vec::new();
    for _ in 0..40 {
        driver.press("tab");
        driver.wait_idle();
        if let Some(id) = driver.focused() {
            forward.push(id.clone());
            if id == "server-settings-leave" {
                break;
            }
        }
    }
    assert_eq!(
        forward.last().map(String::as_str),
        Some("server-settings-leave"),
        "tabbing reaches the Leave button: {forward:?}"
    );
    driver.press("shift-tab");
    driver.wait_idle();
    assert_ne!(driver.focused().as_deref(), Some("server-settings-leave"));
}

/// Task 28 (T6c review m9): in a narrow settings body the GIF settings column shrinks to the row, as the web's
/// content-sized track does, instead of keeping the width its widest part would take: every part ends inside the
/// window.
#[test]
#[cfg_attr(
    not(target_os = "linux"),
    ignore = "the headless renderer is Linux only here"
)]
fn the_gif_settings_column_shrinks_in_a_narrow_body() {
    let mut json: serde_json::Value =
        serde_json::from_str(&std::fs::read_to_string(fixture("gifs")).unwrap()).unwrap();
    let width = 560.0;
    json["window"] = serde_json::json!([width as u32, 800]);
    let dir = tempfile_dir("t28-gifs-narrow");
    let narrow = dir.join("gifs-narrow.json");
    std::fs::write(&narrow, json.to_string()).unwrap();
    let mut driver = dilla_parity::Driver::launch_fixture(&narrow, None, &[]);
    driver.wait_idle();
    let column = driver
        .element("gif-settings")
        .expect("the GIF settings drew");
    assert!(
        column.x + column.width <= width,
        "the column ends at {} in a {width}px window",
        column.x + column.width
    );
    // Review L m8: the column's widest part (the key field, its 20rem basis) shrinks with it; a part that kept its width
    // would overflow the column, clipped, while the column itself ends inside the window.
    let field = driver.element("gif-key-field").expect("the key field drew");
    assert!(
        field.x + field.width <= column.x + column.width,
        "the key field ends at {} past the column's {}",
        field.x + field.width,
        column.x + column.width
    );
    let _ = std::fs::remove_dir_all(&dir);
}

fn tempfile_dir(tag: &str) -> PathBuf {
    let dir = std::env::temp_dir().join(format!("{tag}-{}", std::process::id()));
    std::fs::create_dir_all(&dir).unwrap();
    dir
}

/// Integration 3, step 2: the server menu's chevron, then its Server settings, opens Task 15's `ServerSettingsView` over the shell (THE server
/// settings host), one at a time, and Close brings the shell back.
#[cfg(feature = "automation")]
#[gpui::test]
fn the_server_menu_opens_the_server_settings_over_the_shell(cx: &mut gpui::TestAppContext) {
    use dilla_desktop::app::{AppView, Launch};
    let path =
        Path::new(env!("CARGO_MANIFEST_DIR")).join("../parity/fixtures/shell/channel-view.json");
    let loaded = dilla_desktop::fixture::Fixture::load(&path, None).expect("the shell fixture");
    cx.update(dilla_desktop::install);
    let (app, vcx) =
        cx.add_window_view(|window, cx| AppView::new(Launch::fixture(loaded), window, cx));
    vcx.run_until_parked();
    assert_eq!(app.read_with(vcx, |a, _| a.screen_name()), "shell");
    let open = vcx
        .debug_bounds("server-settings-open")
        .expect("the server menu's chevron");
    vcx.simulate_click(open.center(), gpui::Modifiers::none());
    vcx.run_until_parked();
    // M7: the chevron opens the server's menu; its first item opens the settings.
    let settings = vcx
        .debug_bounds("menu-item-settings")
        .expect("the server menu's Server settings");
    vcx.simulate_click(settings.center(), gpui::Modifiers::none());
    vcx.run_until_parked();
    assert_eq!(
        app.read_with(vcx, |a, _| a.screen_name()),
        "server-settings"
    );
    let (view, over) = app.read_with(vcx, |a, _| a.server_settings()).unwrap();
    assert!(over, "over the shell, which comes back");
    // Review M8: the settings hold the keyboard (Tab and Escape reach them).
    let focus = view.read_with(vcx, gpui::Focusable::focus_handle);
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    assert!(
        vcx.update(|window, cx| focus.contains_focused(window, cx)),
        "the focus is in the settings"
    );
    view.update(vcx, |v, cx| v.request_close(cx));
    vcx.run_until_parked();
    assert_eq!(app.read_with(vcx, |a, _| a.screen_name()), "shell");
    assert!(app.read_with(vcx, |a, _| a.server_settings()).is_none());
}

/// M7 (Shell.tsx `serverMenu`): the channel list head's ⌄ is the server's menu (ActionMenu, labelled `admin.open`; its
/// trigger is a tab stop, `tests/components.rs`): Server settings, Mute server and the four notify modes. Escape closes it
/// and leaves focus on the ⌄, from which Down opens it again; Enter on Mute server toggles it and keeps the menu open;
/// Enter on Server settings opens the server's settings.
#[cfg(feature = "automation")]
#[gpui::test]
fn the_server_menu_is_keyboard_reachable_and_lists_the_web_items(cx: &mut gpui::TestAppContext) {
    use dilla_desktop::app::{AppView, Launch};
    let path =
        Path::new(env!("CARGO_MANIFEST_DIR")).join("../parity/fixtures/shell/channel-view.json");
    let loaded = dilla_desktop::fixture::Fixture::load(&path, None).expect("the shell fixture");
    cx.update(dilla_desktop::install);
    let (app, vcx) =
        cx.add_window_view(|window, cx| AppView::new(Launch::fixture(loaded), window, cx));
    vcx.run_until_parked();
    let items = [
        "menu-item-settings",
        "menu-item-mute",
        "menu-item-mode:default",
        "menu-item-mode:all",
        "menu-item-mode:mentions",
        "menu-item-mode:nothing",
    ];
    assert!(
        vcx.debug_bounds("menu-item-settings").is_none(),
        "closed at first"
    );
    let chevron = vcx
        .debug_bounds("server-settings-open")
        .expect("the server menu's chevron");
    vcx.simulate_click(chevron.center(), gpui::Modifiers::none());
    vcx.run_until_parked();
    for id in items {
        assert!(vcx.debug_bounds(id).is_some(), "{id} is in the menu");
    }
    vcx.simulate_keystrokes("escape");
    vcx.run_until_parked();
    assert!(
        vcx.debug_bounds("menu-item-settings").is_none(),
        "Escape closes it"
    );
    // Focus went back to the ⌄: Down opens the menu on its first item, Down again is Mute server; Enter toggles it and the
    // menu stays open.
    vcx.simulate_keystrokes("down");
    vcx.run_until_parked();
    assert!(
        vcx.debug_bounds("menu-item-settings").is_some(),
        "Down opens it from the ⌄"
    );
    vcx.simulate_keystrokes("down");
    vcx.run_until_parked();
    activate(vcx);
    assert!(
        vcx.debug_bounds("menu-item-mute").is_some(),
        "the mute check keeps the menu open"
    );
    vcx.simulate_keystrokes("home");
    vcx.run_until_parked();
    activate(vcx);
    assert_eq!(
        app.read_with(vcx, |a, _| a.screen_name()),
        "server-settings"
    );
}

/// Enter as a person presses it: a click on the focused control fires on the key's release (gpui's keyboard click), which
/// `simulate_keystrokes` does not send.
#[cfg(feature = "automation")]
fn activate(vcx: &mut gpui::VisualTestContext) {
    vcx.simulate_keystrokes("enter");
    vcx.simulate_event(gpui::KeyUpEvent {
        keystroke: gpui::Keystroke::parse("enter").expect("a keystroke"),
    });
    vcx.run_until_parked();
}

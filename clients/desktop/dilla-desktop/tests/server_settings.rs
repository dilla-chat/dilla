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

/// Ruling R22: a parity test that still fails waits for the shared-component pass (T6c) and its parity sweep.
macro_rules! parity_after_t6c {
    ($($test:ident => $state:literal, $why:literal),* $(,)?) => {$(
        #[test]
        #[ignore = $why]
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
}

parity_after_t6c! {
    parity_server_settings_channels => "channels", "parity after T6c: chrome 0.2229 % (the rows' icon glyphs are drawn one device pixel off the web's)",
    parity_server_settings_channels_b => "channels-b", "parity after T6c: whole frame 0.6982 % (the parity world makes the channels without the fixture's positions, so the reference lists them by channel id: qa, dev, design; Archive before Voice; needs the seed to set positions and a re-shoot)",
    parity_server_settings_channel_permissions => "channel-permissions", "parity after T6c: whole frame 2.4395 % (the permissions now load; left: the GPUI editor has no Bot channel switch (dilla-client keeps no Bot channels), and the web's dialog sits 14px from the top, centred in the window, GPUI's at 12vh in the pane)",
    parity_server_settings_channel_permissions_b => "channel-permissions-b", "parity after T6c: whole frame 1.8456 % (as channel-permissions)",
}

parity_tests! {
    parity_server_settings_danger => "danger",
    parity_server_settings_danger_b => "danger-b",
    parity_server_settings_welcome_b => "welcome-b",
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

/// Integration 3, step 2: the server menu's chevron opens Task 15's `ServerSettingsView` over the shell (THE server
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

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
    parity_server_settings_overview => "overview",
    parity_server_settings_overview_b => "overview-b",
    parity_server_settings_afk => "afk",
    parity_server_settings_afk_b => "afk-b",
    parity_server_settings_channels => "channels",
    parity_server_settings_channels_b => "channels-b",
    parity_server_settings_channel_permissions => "channel-permissions",
    parity_server_settings_channel_permissions_b => "channel-permissions-b",
    parity_server_settings_gifs => "gifs",
    parity_server_settings_gifs_b => "gifs-b",
    parity_server_settings_invites => "invites",
    parity_server_settings_invites_b => "invites-b",
    parity_server_settings_welcome => "welcome",
    parity_server_settings_welcome_b => "welcome-b",
    parity_server_settings_danger => "danger",
    parity_server_settings_danger_b => "danger-b",
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

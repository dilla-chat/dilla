//! Task 14's parity, behaviour and keyboard tests for the members area (standard procedure S4-S6).
//!
//! - The fixture tests (no server, automation build) hold the members and roles facets of the parity fixtures to the
//!   rank rule: what the views would offer the viewer is what `features::roles::rank` allows.
//! - `parity_members_*` and `parity_server_settings_*` compare each fixture with its web reference
//!   (`parity::screen::assert_screen`). They are `#[ignore]`d until their prerequisites land: the references are shot by
//!   `shoot.mjs`, the shell hands
//!   its slot context to `screens::members::sync`, and the settings frame draws the `/c/<id>/settings/<section>` route
//!   (Tasks 8 and 15). Each test names its blocker.
//! - The behaviour tests port e2e specs; they need the same app hosting and a real dillad.

#![cfg(feature = "automation")]

use std::path::{Path, PathBuf};

use dilla_client::state::{CommunityId, UserId};
use dilla_desktop::fixture::Fixture;
use dilla_desktop::screens::members::may_moderate;
use dilla_parity::screen::assert_screen;

fn fixture_path(name: &str) -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/fixtures")
        .join(format!("{name}.json"))
}

fn load(name: &str) -> Fixture {
    Fixture::load(&fixture_path(name), None).unwrap_or_else(|e| panic!("{name}: {e}"))
}

fn user(f: &Fixture, c: CommunityId, username: &str) -> UserId {
    f.state.members[&c]
        .iter()
        .find(|m| m.username == username)
        .unwrap_or_else(|| panic!("{username} is a member"))
        .user_id
}

#[test]
fn the_owner_fixture_may_moderate_everyone_but_herself() {
    let f = load("members/pane");
    let c = f.state.communities[0].id;
    let access = f.state.roles.access_of(c);
    assert!(access.loaded && access.owner && access.kick_members && access.ban_members);
    assert!(may_moderate(&f.state, c, user(&f, c, "mira")));
    assert!(may_moderate(&f.state, c, user(&f, c, "bjorn")));
    assert!(!may_moderate(&f.state, c, user(&f, c, "ada")));
    // The facets land: presence for every member, the role with its holder.
    assert_eq!(f.state.people.presence[&c].len(), 3);
    let crew = f
        .state
        .roles
        .roles_of(c)
        .iter()
        .find(|r| r.name == "crew")
        .unwrap();
    assert_eq!((crew.position, crew.color, crew.members), (1, 0x5A_A8E6, 1));
    assert!(
        f.state.members[&c]
            .iter()
            .any(|m| m.username == "mira" && m.role_ids == [crew.id])
    );
}

#[test]
fn a_moderator_fixture_ranks_below_the_owner_and_above_only_lower_roles() {
    let f = load("members/pane-b");
    let c = f.state.communities[0].id;
    let access = f.state.roles.access_of(c);
    // ada holds role 14 of 15 (kick, ban, moderate, manage roles), mira owns the server.
    assert!(access.loaded && !access.owner);
    assert!(
        access.kick_members && access.ban_members && access.moderate_members && access.manage_roles
    );
    assert_eq!(access.top, 14);
    assert!(
        !may_moderate(&f.state, c, user(&f, c, "mira")),
        "never the owner"
    );
    assert!(
        !may_moderate(&f.state, c, user(&f, c, "ada")),
        "never oneself"
    );
    assert!(
        may_moderate(&f.state, c, user(&f, c, "gunnar")),
        "a lower role"
    );
    assert!(
        !may_moderate(&f.state, c, user(&f, c, "tove")),
        "a role above her own (15)"
    );
    assert_eq!(
        f.state.roles.roles_of(c).len(),
        16,
        "15 roles and @everyone"
    );
    assert_eq!(f.state.members[&c].len(), 12);
}

#[test]
fn every_members_fixture_parses_and_the_pane_ones_open_the_channel() {
    for name in [
        "members/pane",
        "members/profile-card",
        "members/context-menu",
        "members/moderation-dialog",
    ] {
        for b in ["", "-b"] {
            let f = load(&format!("{name}{b}"));
            assert!(f.line().starts_with(&format!("fixture {name}{b} ")));
        }
    }
    for name in ["members", "roles", "self-roles", "bans", "moderation-log"] {
        for b in ["", "-b"] {
            let path = fixture_path(&format!("server-settings/{name}{b}"));
            let text = std::fs::read_to_string(&path).unwrap();
            dilla_client::testing::spec::ScreenFixture::parse(&text)
                .unwrap_or_else(|e| panic!("{}: {e}", path.display()));
        }
    }
}

macro_rules! parity {
    ($($test:ident => $name:literal, $why:literal;)*) => {$(
        #[test]
        #[ignore = $why]
        fn $test() {
            assert_screen(&fixture_path($name))
                .unwrap_or_else(|e| panic!("{} differs from its web reference: {e}", $name));
        }
    )*};
}

parity! {
    parity_members_pane => "members/pane", "needs its reference (shoot.mjs) and the shell handing its slot context to screens::members::sync (Task 8, at merge)";
    parity_members_pane_b => "members/pane-b", "needs its reference (shoot.mjs) and the shell handing its slot context to screens::members::sync (Task 8, at merge)";
    parity_members_profile_card => "members/profile-card", "needs its reference (shoot.mjs) and the shell handing its slot context to screens::members::sync (Task 8, at merge)";
    parity_members_profile_card_b => "members/profile-card-b", "needs its reference (shoot.mjs) and the shell handing its slot context to screens::members::sync (Task 8, at merge)";
    parity_members_context_menu => "members/context-menu", "cannot be shot: shoot.mjs has no shift-f10 (Playwright wants F10) and the chord grammar no contextmenu key (Task 5); also needs members::sync from the shell";
    parity_members_context_menu_b => "members/context-menu-b", "cannot be shot: shoot.mjs has no shift-f10 (Playwright wants F10) and the chord grammar no contextmenu key (Task 5); also needs members::sync from the shell";
    parity_members_moderation_dialog => "members/moderation-dialog", "cannot be shot: the web opens kick only from the person menu (MemberCardHost CARD has no moderation), which no step can open: shoot.mjs has no shift-f10 and the chord grammar no contextmenu key (Task 5)";
    parity_members_moderation_dialog_b => "members/moderation-dialog-b", "cannot be shot: the web opens kick only from the person menu (MemberCardHost CARD has no moderation), which no step can open: shoot.mjs has no shift-f10 and the chord grammar no contextmenu key (Task 5)";
    parity_server_settings_members => "server-settings/members", "needs its reference (shoot.mjs) and the settings route drawn by Tasks 8/15";
    parity_server_settings_members_b => "server-settings/members-b", "needs its reference (shoot.mjs) and the settings route drawn by Tasks 8/15";
    parity_server_settings_roles => "server-settings/roles", "needs its reference (shoot.mjs) and the settings route drawn by Tasks 8/15";
    parity_server_settings_roles_b => "server-settings/roles-b", "needs its reference (shoot.mjs) and the settings route drawn by Tasks 8/15";
    parity_server_settings_self_roles => "server-settings/self-roles", "needs its reference (shoot.mjs) and the settings route drawn by Tasks 8/15";
    parity_server_settings_self_roles_b => "server-settings/self-roles-b", "needs its reference (shoot.mjs) and the settings route drawn by Tasks 8/15";
    parity_server_settings_bans => "server-settings/bans", "needs its reference (shoot.mjs) and the settings route drawn by Tasks 8/15";
    parity_server_settings_bans_b => "server-settings/bans-b", "needs its reference (shoot.mjs) and the settings route drawn by Tasks 8/15";
    parity_server_settings_moderation_log => "server-settings/moderation-log", "needs its reference (shoot.mjs) and the settings route drawn by Tasks 8/15";
    parity_server_settings_moderation_log_b => "server-settings/moderation-log-b", "needs its reference (shoot.mjs) and the settings route drawn by Tasks 8/15";
}

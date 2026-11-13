//! Task 14's parity, behaviour and keyboard tests for the members area (standard procedure S4-S6).
//!
//! - The fixture tests (no server, automation build) hold the members and roles facets of the parity fixtures to the
//!   rank rule: what the views would offer the viewer is what `features::roles::rank` allows.
//! - `parity_members_*` and `parity_server_settings_*` compare each fixture with its web reference
//!   (`parity::screen::assert_screen`). They are `#[ignore]`d until their prerequisites land: the references are shot by
//!   `shoot.mjs`, and the settings host draws the `/c/<id>/settings/<section>` route (Tasks 1b and 15); the two shot
//!   references wait for the T6c parity pass (R22). Each test names its blocker.
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

/// Fix round 1 (1): the role editor disables a bit the manager does not hold (RolesSection.tsx `mine`), and every bit of
/// a role at or above her rank.
#[test]
fn a_manager_cannot_toggle_a_bit_they_lack() {
    use dilla_client::features::roles::PermLock;
    use dilla_client::features::roles::perm::{self, PermKey};
    use dilla_desktop::screens::members::{my_bits, permission_locks};
    let f = load("members/pane-b");
    let c = f.state.communities[0].id;
    let mine = my_bits(&f.state, c);
    assert_ne!(mine, perm::PERM_ALL);
    assert!(mine & perm::BAN_MEMBERS != 0 && mine & perm::ADMINISTRATOR == 0);
    let roles = f.state.roles.roles_of(c).to_vec();
    let low = roles.iter().find(|r| r.name == "role number 01").unwrap();
    let locks = permission_locks(&f.state, c, low);
    let lock = |k: PermKey| locks.iter().find(|(key, _)| *key == k).unwrap().1;
    assert_eq!(lock(PermKey::Ban), None, "a held bit may be toggled");
    assert_eq!(lock(PermKey::Admin), Some(PermLock::NotHeld));
    assert_eq!(lock(PermKey::ManageServer), Some(PermLock::NotHeld));
    let high = roles
        .iter()
        .find(|r| r.name == "harbour masters of the northern fleet")
        .unwrap();
    assert!(
        permission_locks(&f.state, c, high)
            .iter()
            .all(|(_, l)| *l == Some(PermLock::Locked))
    );
    // The owner holds everything.
    let f = load("members/pane");
    let c = f.state.communities[0].id;
    assert_eq!(my_bits(&f.state, c), perm::PERM_ALL);
}

/// Fix round 1 (4): MemberRolesFlow's list: highest first without @everyone, rank and bit locks, and nothing once the
/// target has left.
#[test]
fn the_assign_dialog_locks_by_rank_and_bits_and_ends_when_the_target_leaves() {
    use dilla_client::features::roles::AssignLock;
    use dilla_desktop::screens::members::assign_items;
    let mut f = load("members/pane-b");
    let c = f.state.communities[0].id;
    let gunnar = user(&f, c, "gunnar");
    let items = assign_items(&f.state, c, gunnar).expect("gunnar is a member");
    assert_eq!(items.len(), 15);
    assert!(items.windows(2).all(|w| w[0].0.position >= w[1].0.position));
    let by = |name: &str| {
        items
            .iter()
            .find(|(r, _, _)| r.name == name)
            .unwrap()
            .clone()
    };
    assert_eq!(
        by("harbour masters of the northern fleet").2,
        Some(AssignLock::Rank)
    );
    assert_eq!(
        by("role number 14").2,
        Some(AssignLock::Rank),
        "her own top role"
    );
    assert_eq!(by("role number 01").2, None);
    assert!(by("role number 11").1, "gunnar holds role 11");
    f.state
        .members
        .get_mut(&c)
        .unwrap()
        .retain(|m| m.user_id != gunnar);
    assert!(assign_items(&f.state, c, gunnar).is_none());
}

/// Fix round 1 (3): deleting a role asks first (RolesSection.tsx DeleteRole): the request opens the confirmation and
/// sends nothing; cancelling closes it.
#[gpui::test]
fn deleting_a_role_goes_through_its_confirmation(cx: &mut gpui::TestAppContext) {
    use dilla_desktop::screens::members::{
        cancel_role_delete, pending_role_delete, request_role_delete,
    };
    let c = CommunityId([7; 16]);
    cx.update(|cx| {
        assert_eq!(pending_role_delete(cx), None);
        request_role_delete(c, [9; 16], "crew".into(), cx);
        assert_eq!(pending_role_delete(cx), Some((c, [9; 16])));
        cancel_role_delete(cx);
        assert_eq!(pending_role_delete(cx), None);
    });
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
    ($($test:ident => $name:literal $(, $why:literal)?;)*) => {$(
        #[test]
        $(#[ignore = $why])?
        fn $test() {
            assert_screen(&fixture_path($name))
                .unwrap_or_else(|e| panic!("{} differs from its web reference: {e}", $name));
        }
    )*};
}

parity! {
    parity_members_pane => "members/pane";
    parity_members_pane_b => "members/pane-b", "needs its reference (shoot.mjs)";
    parity_members_profile_card => "members/profile-card", "parity after T6c: chrome 0.3377 % (half-px snap: Chromium paints box edges at whole css px, half up; the card edge)";
    parity_members_profile_card_b => "members/profile-card-b", "needs its reference (shoot.mjs)";
    parity_members_context_menu => "members/context-menu", "cannot be shot: shoot.mjs has no shift-f10 (Playwright wants F10) and the chord grammar no contextmenu key (Task 5)";
    parity_members_context_menu_b => "members/context-menu-b", "cannot be shot: shoot.mjs has no shift-f10 (Playwright wants F10) and the chord grammar no contextmenu key (Task 5)";
    parity_members_moderation_dialog => "members/moderation-dialog", "cannot be shot: the web opens kick only from the person menu (MemberCardHost CARD has no moderation), which no step can open: shoot.mjs has no shift-f10 and the chord grammar no contextmenu key (Task 5)";
    parity_members_moderation_dialog_b => "members/moderation-dialog-b", "cannot be shot: the web opens kick only from the person menu (MemberCardHost CARD has no moderation), which no step can open: shoot.mjs has no shift-f10 and the chord grammar no contextmenu key (Task 5)";
    parity_server_settings_members => "server-settings/members", "needs its reference (shoot.mjs) and the settings route drawn by the settings host (Tasks 1b/15)";
    parity_server_settings_members_b => "server-settings/members-b", "needs its reference (shoot.mjs) and the settings route drawn by the settings host (Tasks 1b/15)";
    parity_server_settings_roles => "server-settings/roles", "needs its reference (shoot.mjs) and the settings route drawn by the settings host (Tasks 1b/15)";
    parity_server_settings_roles_b => "server-settings/roles-b", "needs its reference (shoot.mjs) and the settings route drawn by the settings host (Tasks 1b/15)";
    parity_server_settings_self_roles => "server-settings/self-roles", "needs its reference (shoot.mjs) and the settings route drawn by the settings host (Tasks 1b/15)";
    parity_server_settings_self_roles_b => "server-settings/self-roles-b", "needs its reference (shoot.mjs) and the settings route drawn by the settings host (Tasks 1b/15)";
    parity_server_settings_bans => "server-settings/bans", "needs its reference (shoot.mjs) and the settings route drawn by the settings host (Tasks 1b/15)";
    parity_server_settings_bans_b => "server-settings/bans-b", "needs its reference (shoot.mjs) and the settings route drawn by the settings host (Tasks 1b/15)";
    parity_server_settings_moderation_log => "server-settings/moderation-log", "needs its reference (shoot.mjs) and the settings route drawn by the settings host (Tasks 1b/15)";
    parity_server_settings_moderation_log_b => "server-settings/moderation-log-b", "needs its reference (shoot.mjs) and the settings route drawn by the settings host (Tasks 1b/15)";
}

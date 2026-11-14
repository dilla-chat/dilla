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

mod support;

use std::path::{Path, PathBuf};
use std::time::Duration;

use dilla_client::state::{CommunityId, UserId};
use dilla_desktop::fixture::Fixture;
use dilla_desktop::screens::members::may_moderate;
use dilla_parity::Driver;
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
    let w = cx
        .add_empty_window()
        .update(|window, _| window.window_handle().window_id());
    let other = cx
        .add_empty_window()
        .update(|window, _| window.window_handle().window_id());
    cx.update(|cx| {
        assert_eq!(pending_role_delete(w, cx), None);
        request_role_delete(w, c, [9; 16], "crew".into(), cx);
        assert_eq!(pending_role_delete(w, cx), Some((c, [9; 16])));
        // Final review sweep: the confirmation is its window's; another window has none, and its cancel leaves it.
        assert_eq!(pending_role_delete(other, cx), None);
        cancel_role_delete(other, cx);
        assert_eq!(pending_role_delete(w, cx), Some((c, [9; 16])));
        cancel_role_delete(w, cx);
        assert_eq!(pending_role_delete(w, cx), None);
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

// ---- the Roles section (RolesSection.tsx): its view of the fixtures, and the shipping window ----

fn hex(id: &[u8]) -> String {
    id.iter().map(|b| format!("{b:02x}")).collect()
}

/// RolesSection.tsx `orderRoles` and the open role: @everyone first, then the roles from the highest down; with nothing
/// chosen the top role below @everyone opens (the web's two panes with the top role's editor open).
#[test]
fn the_roles_section_lists_everyone_first_and_opens_the_top_role() {
    use dilla_desktop::screens::members::roles::RolesView;
    let f = load("server-settings/roles-b");
    let c = f.state.communities[0].id;
    let view = RolesView::of(&f.state, c, None, None);
    let names: Vec<&str> = view.ordered.iter().map(|r| r.name.as_str()).collect();
    assert_eq!(names[0], "@everyone");
    assert_eq!(names[1], "harbour masters of the northern fleet");
    assert_eq!(names[2], "role number 14");
    assert_eq!(names[15], "role number 01");
    assert_eq!(
        view.role.as_ref().map(|r| r.name.as_str()),
        Some("harbour masters of the northern fleet")
    );
    // A chosen role opens while it is listed; one that is gone falls back to the top role.
    let low = view.ordered[15].id;
    let chosen = RolesView::of(&f.state, c, Some(low), None);
    assert_eq!(chosen.role.as_ref().map(|r| r.id), Some(low));
    let gone = RolesView::of(&f.state, c, Some([0xEE; 16]), None);
    assert_eq!(gone.role.as_ref().map(|r| r.name.as_str()), Some(names[1]));
    let f = load("server-settings/roles");
    let c = f.state.communities[0].id;
    let view = RolesView::of(&f.state, c, None, None);
    assert_eq!(view.role.as_ref().map(|r| r.name.as_str()), Some("crew"));
}

/// The rank rule (RolesSection.tsx `locked`, `editable`) and the grid by the one resolver (`mine`): ada ranks at role 14
/// of 15 (manage roles, kick, ban, moderate) under mira, the owner. Role 15 and her own role 14 are at or above her rank:
/// read-only, every switch locked, no menu, no name or colour change. Role 01 is below her: the bits she holds may change,
/// the ones she lacks are locked as not held; the patches carry exactly the change.
#[test]
fn the_roles_section_locks_at_or_above_my_rank_and_the_bits_i_lack() {
    use dilla_client::features::roles::PermLock;
    use dilla_client::features::roles::perm::{self, PermKey};
    use dilla_desktop::screens::members::roles::{NameProblem, RoleMenu, RolesView};
    let f = load("server-settings/roles-b");
    let c = f.state.communities[0].id;
    let by = |name: &str| {
        RolesView::of(&f.state, c, None, None)
            .ordered
            .into_iter()
            .find(|r| r.name == name)
            .unwrap()
    };
    for name in ["harbour masters of the northern fleet", "role number 14"] {
        let view = RolesView::of(&f.state, c, Some(by(name).id), None);
        assert!(
            view.can_manage && !view.editable,
            "{name} is at or above her rank"
        );
        let role = view.role.clone().unwrap();
        assert!(
            view.permission_rows(role.allow)
                .iter()
                .flat_map(|(_, rows)| rows)
                .all(|(_, _, lock)| *lock == Some(PermLock::Locked))
        );
        assert_eq!(
            view.permission_patch(role.allow, PermKey::View, false),
            None
        );
        assert_eq!(view.name_patch("renamed"), Ok(None));
        assert_eq!(view.color_patch(role.color, 0x5A_A8E6), None);
        assert!(view.menu(role.id).is_empty());
    }
    let low = by("role number 01");
    let view = RolesView::of(&f.state, c, Some(low.id), None);
    assert!(view.editable);
    let rows = view.permission_rows(low.allow);
    let lock = |k: PermKey| {
        rows.iter()
            .flat_map(|(_, r)| r)
            .find(|(key, _, _)| *key == k)
            .unwrap()
            .2
    };
    assert_eq!(lock(PermKey::Kick), None, "a bit she holds");
    assert_eq!(lock(PermKey::ManageChannels), Some(PermLock::NotHeld));
    assert_eq!(lock(PermKey::Admin), Some(PermLock::NotHeld));
    let patch = view
        .permission_patch(low.allow, PermKey::Kick, true)
        .unwrap();
    assert_eq!(patch.allow, Some(low.allow | perm::KICK_MEMBERS));
    assert_eq!(
        view.permission_patch(low.allow, PermKey::ManageChannels, true),
        None
    );
    assert_eq!(
        view.name_patch("  deck hands  ")
            .unwrap()
            .unwrap()
            .name
            .as_deref(),
        Some("deck hands")
    );
    assert_eq!(view.name_patch("role number 01"), Ok(None));
    assert_eq!(view.name_patch("   "), Err(NameProblem::Empty));
    assert_eq!(view.name_patch(&"é".repeat(51)), Err(NameProblem::Long));
    assert_eq!(
        view.color_patch(low.color, 0x7C_FF8E).unwrap().color,
        Some(0x7C_FF8E)
    );
    // Role 01 is the lowest she may move: up, not down.
    assert_eq!(
        view.menu(low.id),
        [
            RoleMenu::Up,
            RoleMenu::Hoist(false),
            RoleMenu::Mentionable(false),
            RoleMenu::Delete
        ]
    );
    // @everyone has a name and no colour of its own.
    let everyone = view.ordered[0].clone();
    let view = RolesView::of(&f.state, c, Some(everyone.id), None);
    assert!(view.editable);
    assert_eq!(view.name_patch("all"), Ok(None));
    assert_eq!(view.color_patch(0, 0x7C_FF8E), None);
    assert!(view.menu(everyone.id).is_empty());
}

/// RolesSection.tsx in the shipping window (the roles-b fixture): the top role opens first, every switch locked; a click
/// on role 01 opens it, where a bit ada holds may change and one she lacks stays locked; the arrows walk the list.
#[test]
fn the_roles_section_selects_a_role_and_locks_by_rank_in_the_window() {
    let f = load("server-settings/roles-b");
    let c = f.state.communities[0].id;
    let roles = f.state.roles.roles_of(c).to_vec();
    let id_of = |name: &str| hex(&roles.iter().find(|r| r.name == name).unwrap().id);
    let (top, low) = (
        id_of("harbour masters of the northern fleet"),
        id_of("role number 01"),
    );
    let mut app = Driver::launch_fixture(&fixture_path("server-settings/roles-b"), None, &[]);
    app.wait_for(&format!("role-open-{top}"), Duration::from_secs(60));
    app.wait_idle();
    assert!(app.element("perm-view-locked").is_some());
    assert!(app.element("perm-kick-locked").is_some());
    app.click(&format!("role-{low}"));
    app.wait_idle();
    assert!(app.element(&format!("role-open-{low}")).is_some());
    assert!(app.element(&format!("role-open-{top}")).is_none());
    assert!(app.element("perm-kick-locked").is_none(), "she holds kick");
    assert!(app.element("perm-view-on").is_some());
    assert!(app.element("perm-manageChannels-locked").is_some());
    // Up from role 01 reaches role 02; Down wraps past "New role" to @everyone.
    app.press("up");
    app.wait_idle();
    assert_eq!(
        app.focused().as_deref(),
        Some(format!("role-{}", id_of("role number 02")).as_str())
    );
}

/// e2e/web/roles.spec.ts (the role editor half), against a real dillad: a manager whose role ranks second of three. The
/// section's view of her client opens the top role read-only and sends nothing for it; the instance refuses the same
/// change made around the view (403), so the gate is advisory and the rank rule is the instance's. Below her rank, the
/// view's patches (a permission she holds, a name, a colour) save through the client roles API and reach the owner; a
/// bit she lacks is locked in the view and refused by the instance.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn the_roles_section_edits_below_my_rank_and_is_refused_above_it() {
    use dilla_client::features::roles::PermLock;
    use dilla_client::features::roles::perm::{self, PermKey};
    use dilla_client::state::AppState;
    use dilla_desktop::screens::members::roles::RolesView;
    use support::world;

    let host = tokio::task::spawn_blocking(|| world::start_host("w2e-roles", "127.0.0.1:0", None))
        .await
        .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let owner = world::sign_up(&host, &invite, "olof", "Olof", "w2e-roles").await;
    let ada = world::sign_up(&host, &invite, "ada", "Ada", "w2e-roles").await;
    let (host, owner, ada, c) = tokio::task::spawn_blocking(move || {
        let c = world::community(&host, &owner, "crew", &[&ada]);
        world::clear_session(&owner);
        world::clear_session(&ada);
        (host, owner, ada, CommunityId(c))
    })
    .await
    .unwrap();
    let (olof, ada_client) = (world::start(&owner).await, world::start(&ada).await);
    let within = Duration::from_secs(30);
    world::wait_for(&olof, within, "the member list", |s| {
        s.members.get(&c).is_some_and(|m| m.len() == 2)
    })
    .await;
    olof.roles().load_access(c).await.unwrap();
    // The owner makes three roles (each where the client's `create_position` puts it), then reads their ranks: the lowest
    // is "low", the middle one becomes ada's (manage roles and kick), the highest is "high".
    // Each waits for the one before it in the owner's view: the client places a role by the roles it holds.
    for (n, name) in ["one", "two", "three"].into_iter().enumerate() {
        olof.roles()
            .create_role(c, name, 0x5A_A8E6, false, false, perm::VIEW_CHANNEL)
            .await
            .unwrap();
        world::wait_for(&olof, within, "the new role", |s| {
            s.roles.roles_of(c).len() == n + 2
        })
        .await;
    }
    let ranked = world::wait_for(&olof, within, "the three roles", |s| {
        s.roles.roles_of(c).len() == 4
    })
    .await;
    let mut made: Vec<_> = ranked
        .roles
        .roles_of(c)
        .iter()
        .filter(|r| r.position > 0)
        .map(|r| (r.position, r.id))
        .collect();
    made.sort();
    let (low, mods, high) = (made[0].1, made[1].1, made[2].1);
    olof.roles()
        .update_role(
            c,
            mods,
            dilla_client::features::roles::RolePatch {
                allow: Some(perm::VIEW_CHANNEL | perm::MANAGE_ROLES | perm::KICK_MEMBERS),
                ..Default::default()
            },
        )
        .await
        .unwrap();
    olof.roles()
        .grant_role(c, UserId(ada.user_id), mods)
        .await
        .unwrap();

    // Ada's client reads her access: rank 2, manage roles.
    world::wait_for(&ada_client, within, "ada's member list", |s| {
        s.members.get(&c).is_some_and(|m| m.len() == 2)
    })
    .await;
    let loaded = |s: &AppState| {
        let a = s.roles.access_of(c);
        a.loaded
            && a.manage_roles
            && a.kick_members
            && s.roles.roles_of(c).len() == 4
            && s.members[&c]
                .iter()
                .any(|m| m.user_id.0 == ada.user_id && m.role_ids.contains(&mods))
    };
    let mut s = None;
    for _ in 0..20 {
        // The section's own read (`ensure_loaded`): the member list, then the access with the roles.
        ada_client.members().open(c).await.unwrap();
        ada_client.roles().load_access(c).await.unwrap();
        let now = ada_client.state().borrow().clone();
        if loaded(&now) {
            s = Some(now);
            break;
        }
        tokio::time::sleep(Duration::from_millis(250)).await;
    }
    let s = s.unwrap_or_else(|| {
        let now = ada_client.state().borrow().clone();
        panic!(
            "ada's access with her role: {:?}, {} roles",
            now.roles.access_of(c),
            now.roles.roles_of(c).len()
        )
    });

    // The top role opens first, read-only: the view sends nothing, and the instance refuses it anyway.
    let view = RolesView::of(&s, c, None, None);
    let open = view.role.clone().unwrap();
    assert_eq!(open.id, high);
    assert!(!view.editable);
    assert_eq!(view.permission_patch(open.allow, PermKey::Kick, true), None);
    let refused = ada_client
        .roles()
        .update_role(
            c,
            high,
            dilla_client::features::roles::RolePatch {
                allow: Some(perm::VIEW_CHANNEL | perm::KICK_MEMBERS),
                ..Default::default()
            },
        )
        .await
        .unwrap_err();
    assert_eq!(refused.status, 403, "{refused}");
    // Her own role is at her rank: read-only too.
    assert!(!RolesView::of(&s, c, Some(mods), None).editable);

    // Below her rank: a bit she holds saves, a bit she lacks is locked and refused.
    let view = RolesView::of(&s, c, Some(low), None);
    assert!(
        view.editable,
        "{:?} top {} roles {:?}",
        view.role,
        view.top,
        s.roles.roles_of(c)
    );
    let role = view.role.clone().unwrap();
    let kick = view
        .permission_patch(role.allow, PermKey::Kick, true)
        .expect("she holds kick");
    ada_client.roles().update_role(c, low, kick).await.unwrap();
    let lock = view
        .permission_rows(role.allow)
        .into_iter()
        .flat_map(|(_, rows)| rows)
        .find(|(k, _, _)| *k == PermKey::ManageChannels)
        .unwrap()
        .2;
    assert_eq!(lock, Some(PermLock::NotHeld));
    assert_eq!(
        view.permission_patch(role.allow, PermKey::ManageChannels, true),
        None
    );
    let lacked = ada_client
        .roles()
        .update_role(
            c,
            low,
            dilla_client::features::roles::RolePatch {
                allow: Some(role.allow | perm::MANAGE_CHANNELS),
                ..Default::default()
            },
        )
        .await
        .unwrap_err();
    assert_eq!(lacked.status, 403, "{lacked}");
    let name = view.name_patch(" deck hands ").unwrap().unwrap();
    ada_client.roles().update_role(c, low, name).await.unwrap();
    let colour = view.color_patch(role.color, 0x7C_FF8E).unwrap();
    ada_client
        .roles()
        .update_role(c, low, colour)
        .await
        .unwrap();

    // The owner sees each change the section saved.
    let mut seen = false;
    for _ in 0..40 {
        olof.roles().load_access(c).await.unwrap();
        let s = olof.state().borrow().clone();
        if s.roles.roles_of(c).iter().any(|r| {
            r.id == low
                && r.name == "deck hands"
                && r.color == 0x7C_FF8E
                && r.allow == perm::VIEW_CHANNEL | perm::KICK_MEMBERS
        }) {
            seen = true;
            break;
        }
        tokio::time::sleep(Duration::from_millis(250)).await;
    }
    assert!(
        seen,
        "the owner sees low renamed, recoloured and given kick"
    );
    olof.stop();
    ada_client.stop();
    drop(host);
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
    parity_members_pane_b => "members/pane-b";
    parity_members_profile_card => "members/profile-card";
    parity_members_profile_card_b => "members/profile-card-b";
    parity_members_context_menu => "members/context-menu";
    parity_members_context_menu_b => "members/context-menu-b";
    parity_members_moderation_dialog => "members/moderation-dialog";
    parity_members_moderation_dialog_b => "members/moderation-dialog-b";
    parity_server_settings_members => "server-settings/members";
    parity_server_settings_members_b => "server-settings/members-b";
    parity_server_settings_roles => "server-settings/roles", "parity after T6c: chrome 0.2172 % (lane E drew RolesSection.tsx's two panes; the residue is the permission switches: each single-line row's switch sits 1 css px lower than the web's, though it is where the CSS puts it (an isolated Chromium render of the same rule set agrees with GPUI); the two-line row matches)";
    parity_server_settings_roles_b => "server-settings/roles-b", "parity after T6c: chrome 0.3006 % (as roles: the switches 1 css px low; the overflowing role row is drawn as the web draws it)";
    parity_server_settings_self_roles => "server-settings/self-roles", "parity after T28 wave 2: whole frame 1.0478 % (SelfRolesSection.tsx's layout is drawn: the fieldset of role checkboxes and the Publish-a-role-panel form; what is left is the browser's own widgets: the groove-edged fieldset sits 1px higher and 1px further left, the native checkbox and the native <select> in its UA sans face)";
    parity_server_settings_self_roles_b => "server-settings/self-roles-b", "parity after T28 wave 2: whole frame 1.9086 % (as self-roles, and the browser wraps the checkbox labels inline mid-label, 'role number / 05', where GPUI wraps whole label boxes)";
    parity_server_settings_bans => "server-settings/bans";
    parity_server_settings_bans_b => "server-settings/bans-b";
    parity_server_settings_moderation_log => "server-settings/moderation-log";
    parity_server_settings_moderation_log_b => "server-settings/moderation-log-b";
}

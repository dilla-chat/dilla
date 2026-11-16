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

mod support;

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
        #[cfg_attr(not(any(target_os = "linux", target_os = "macos")), ignore = "references are Linux and macOS only")]
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

/// Mac report (2026-11-16): a person's click on the server menu's chevron opened nothing. A person's pointer arrives
/// first and rests (the hover style and the tooltip's timer run, the tooltip shows) before it presses; the test above
/// presses with no pointer move. A second click on the chevron closes the menu again.
#[cfg(feature = "automation")]
#[gpui::test]
fn a_persons_click_on_the_server_chevron_opens_and_closes_the_menu(cx: &mut gpui::TestAppContext) {
    use dilla_desktop::app::{AppView, Launch};
    let path =
        Path::new(env!("CARGO_MANIFEST_DIR")).join("../parity/fixtures/shell/channel-view.json");
    let loaded = dilla_desktop::fixture::Fixture::load(&path, None).expect("the shell fixture");
    cx.update(dilla_desktop::install);
    let (_app, vcx) =
        cx.add_window_view(|window, cx| AppView::new(Launch::fixture(loaded), window, cx));
    vcx.run_until_parked();
    let chevron = vcx
        .debug_bounds("server-settings-open")
        .expect("the server menu's chevron")
        .center();
    person_click(vcx, chevron);
    assert!(
        vcx.debug_bounds("menu-item-settings").is_some(),
        "a click on the chevron opens the server menu"
    );
    person_click(vcx, chevron);
    assert!(
        vcx.debug_bounds("menu-item-settings").is_none(),
        "a second click closes it"
    );
}

/// The invite panel's "Expires after" and "Uses" choices take a person's click (Mac report, 2026-11-16: they kept 7 days
/// and 10).
#[cfg(feature = "automation")]
#[gpui::test]
fn a_click_chooses_the_invites_expiry_and_uses(cx: &mut gpui::TestAppContext) {
    use dilla_desktop::app::{AppView, Launch};
    let loaded = dilla_desktop::fixture::Fixture::load(&fixture("invites"), None)
        .expect("the invites fixture");
    cx.update(dilla_desktop::install);
    let (_app, vcx) =
        cx.add_window_view(|window, cx| AppView::new(Launch::fixture(loaded), window, cx));
    vcx.run_until_parked();
    for (option, now, was) in [
        (
            "segmented-invite-ttl-3600",
            "segmented-invite-ttl-3600-checked",
            "segmented-invite-ttl-604800-checked",
        ),
        (
            "segmented-invite-uses-choice-1",
            "segmented-invite-uses-choice-1-checked",
            "segmented-invite-uses-choice-10-checked",
        ),
    ] {
        assert!(vcx.debug_bounds(was).is_some(), "{was} at first");
        let at = vcx
            .debug_bounds(option)
            .unwrap_or_else(|| panic!("{option} drew"))
            .center();
        person_click(vcx, at);
        assert!(vcx.debug_bounds(now).is_some(), "a click checks {option}");
        assert!(
            vcx.debug_bounds(was).is_none(),
            "{was} is no longer checked"
        );
    }
}

/// A person's click: the pointer moves there and rests past the tooltip delay, then presses and lets go.
#[cfg(feature = "automation")]
fn person_click(vcx: &mut gpui::VisualTestContext, at: gpui::Point<gpui::Pixels>) {
    vcx.simulate_mouse_move(at, None, gpui::Modifiers::none());
    vcx.executor()
        .advance_clock(std::time::Duration::from_millis(600));
    vcx.run_until_parked();
    vcx.simulate_click(at, gpui::Modifiers::none());
    vcx.run_until_parked();
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

/// Package 10b: a channel private from birth, made through the create dialog's flow against a real dillad.
mod live {
    use std::time::Duration;

    use dilla_client::features::roles::perm;
    use dilla_client::features::signup::NewChannelKind;
    use dilla_client::state::{CommunityId, UserId};
    use dilla_desktop::screens::create::flows::{
        PRIVATE_ROLES_MAX, offered_roles, role_rows, toggle_role,
    };

    use super::support::world;

    const WITHIN: Duration = Duration::from_secs(30);

    /// e2e/web/private.spec.ts (create): the owner makes a role for Ada, opens a new text channel with **private channel**
    /// on and the role ticked (Create.tsx: only roles under the creator's top, @everyone never), and creates it. Ada, who
    /// holds the role, sees the channel; Björn, who does not, never does, while a public channel made after it reaches
    /// him. The 98-role cap disables the rows past it.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    #[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
    async fn create_private_channel_with_roles() {
        let tag = "p10b-private";
        let host = tokio::task::spawn_blocking(move || world::start_host(tag, "127.0.0.1:0", None))
            .await
            .unwrap();
        let invite = host.invites.last().unwrap().clone();
        let olof = world::sign_up(&host, &invite, "olof", "Olof", tag).await;
        let ada = world::sign_up(&host, &invite, "ada", "Ada", tag).await;
        let bjorn = world::sign_up(&host, &invite, "bjorn", "Björn", tag).await;
        let (_host, olof, ada, bjorn, c) = tokio::task::spawn_blocking(move || {
            let c = world::community(&host, &olof, "crew", &[&ada, &bjorn]);
            for p in [&olof, &ada, &bjorn] {
                world::clear_session(p);
            }
            (host, olof, ada, bjorn, CommunityId(c))
        })
        .await
        .unwrap();
        let ada_id = UserId(ada.user_id);
        let (owner, ada, bjorn) = (
            world::start(&olof).await,
            world::start(&ada).await,
            world::start(&bjorn).await,
        );
        for client in [&owner, &ada, &bjorn] {
            world::wait_for(client, WITHIN, "the member list", |s| {
                s.members.get(&c).is_some_and(|m| m.len() == 3)
            })
            .await;
        }
        owner.roles().load_access(c).await.unwrap();
        let crew = owner
            .roles()
            .create_role(c, "crew-mates", 0x5A_A8E6, false, false, perm::VIEW_CHANNEL)
            .await
            .unwrap();
        owner.roles().grant_role(c, ada_id, crew).await.unwrap();
        owner.roles().load_access(c).await.unwrap();
        let s = world::wait_for(&owner, WITHIN, "the new role", |s| {
            s.roles.roles_of(c).iter().any(|r| r.id == crew)
        })
        .await;

        // What the dialog offers the owner: the role, never @everyone; a tick checks its row.
        let access = s.roles.access_of(c);
        assert!(access.manage_roles, "the owner gets the private switch");
        let offered = offered_roles(s.roles.roles_of(c), &access);
        assert_eq!(
            offered.iter().map(|r| r.id).collect::<Vec<_>>(),
            vec![crew],
            "every role below the owner's top but @everyone"
        );
        let mut ticked = Vec::new();
        let key: String = crew.iter().map(|b| format!("{b:02x}")).collect();
        toggle_role(&mut ticked, &offered, &key, true);
        toggle_role(&mut ticked, &offered, "not-a-role", true);
        assert_eq!(ticked, vec![crew]);
        let rows = role_rows(&offered, &ticked, PRIVATE_ROLES_MAX, Some("full"));
        assert!(rows[0].checked && !rows[0].disabled);
        // At the cap, an unticked row is disabled with its reason.
        let rows = role_rows(&offered, &[], 0, Some("full"));
        assert!(rows[0].disabled && rows[0].detail.as_deref() == Some("full"));

        let secret = owner
            .onboarding()
            .create_channel_with(c, NewChannelKind::Text, "secret", None, &[], Some(&ticked))
            .await
            .unwrap();
        // A private announcement channel is refused before anything is sent.
        assert!(
            owner
                .onboarding()
                .create_channel_with(
                    c,
                    NewChannelKind::Announcement,
                    "news",
                    None,
                    &[],
                    Some(&ticked)
                )
                .await
                .is_err()
        );
        owner
            .onboarding()
            .create_channel_with(c, NewChannelKind::Text, "lobby", None, &[], None)
            .await
            .unwrap();
        for client in [&owner, &ada, &bjorn] {
            client.refresh();
        }
        world::wait_for(&ada, WITHIN, "the private channel for its role", |s| {
            s.channel(secret).is_some()
        })
        .await;
        let s = world::wait_for(&bjorn, WITHIN, "the public channel", |s| {
            s.channel_named(c, "lobby").is_some()
        })
        .await;
        assert!(
            s.channel(secret).is_none() && s.channel_named(c, "secret").is_none(),
            "a member without the role never sees the private channel"
        );
    }
}

/// Package 10d (ServerSettings.tsx's access effect): the person's access changes while server settings are open on Bans.
/// When ban members goes, the settings fall back to the first section still allowed, the unsaved draft goes, and
/// `workspace.permissionChanged` is said once (a later state with the same access says nothing again); a lost read of the
/// access says it too.
#[gpui::test]
fn permission_change_is_announced(cx: &mut gpui::TestAppContext) {
    use std::cell::RefCell;
    use std::rc::Rc;
    use std::sync::Arc;

    use dilla_client::state::roles::CommunityAccess;
    use dilla_client::state::{AppState, Community, CommunityId, UserId};
    use dilla_desktop::screens::server_settings::{
        AccessChange, ServerSettingsView, SettingsEvent, access_change,
    };
    use dilla_desktop::screens::shell::Clock;

    let community = CommunityId([4; 16]);
    let state_with = |access: CommunityAccess| {
        let mut s = AppState::default();
        s.communities.push(Community {
            id: community,
            name: "Crew".into(),
            owner: UserId([1; 16]),
            policy_version: 1,
        });
        s.roles.access.insert(community, access);
        Arc::new(s)
    };
    let moderator = CommunityAccess {
        loaded: true,
        ban_members: true,
        create_invite: true,
        ..CommunityAccess::default()
    };
    let demoted = CommunityAccess {
        ban_members: false,
        ..moderator.clone()
    };

    // The rule alone.
    let access = |a: &CommunityAccess| dilla_client::state::server_admin::Access::from(a);
    assert_eq!(
        access_change(
            Some(ServerSection::Bans),
            Some(ServerSection::Bans),
            None,
            &access(&demoted)
        ),
        AccessChange {
            clear: true,
            announce: true
        }
    );
    assert_eq!(
        access_change(
            Some(ServerSection::Members),
            Some(ServerSection::Bans),
            Some(ServerSection::Bans),
            &access(&demoted)
        ),
        AccessChange::default(),
        "said once per lost section"
    );
    assert_eq!(
        access_change(
            Some(ServerSection::Bans),
            Some(ServerSection::Bans),
            None,
            &access(&moderator)
        ),
        AccessChange::default()
    );

    // The open view.
    cx.update(dilla_desktop::install);
    let said = Rc::new(RefCell::new(Vec::<String>::new()));
    let (view, vcx) = cx.add_window_view(|window, cx| {
        ServerSettingsView::new(
            community,
            state_with(moderator.clone()),
            None,
            Clock::Fixed {
                now: 1_800_000_000,
                utc: true,
            },
            Some(ServerSection::Bans),
            window,
            cx,
        )
    });
    {
        let said = said.clone();
        vcx.update(|_, cx| {
            cx.subscribe(&view, move |_, e: &SettingsEvent, _| {
                if let SettingsEvent::Announce(m) = e {
                    said.borrow_mut().push(m.clone());
                }
            })
            .detach();
        });
    }
    vcx.run_until_parked();
    assert_eq!(
        view.read_with(vcx, |v, _| v.active()),
        Some(ServerSection::Bans)
    );
    let changed = dilla_desktop::strings::t("workspace.permissionChanged");
    // Read to a screen reader only (ServerSettings.tsx's polite live region), never a toast or other visible text.
    let read = |vcx: &mut gpui::VisualTestContext| {
        vcx.update(|window, cx| {
            (
                dilla_desktop_ui::a11y::announcement_seq(window, cx),
                dilla_desktop_ui::a11y::last_announcement(window, cx),
            )
        })
    };
    let (before, _) = read(vcx);
    view.update(vcx, |v, cx| v.set_state(state_with(demoted.clone()), cx));
    vcx.run_until_parked();
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    let (first, text) = read(vcx);
    assert_eq!(text.as_deref(), Some(changed));
    assert_ne!(first, before);
    assert!(said.borrow().is_empty(), "no toast");
    assert_eq!(
        view.read_with(vcx, |v, _| v.active()),
        Some(ServerSection::Members),
        "the first section still allowed"
    );
    view.update(vcx, |v, cx| v.set_state(state_with(demoted.clone()), cx));
    vcx.run_until_parked();
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    assert_eq!(read(vcx).0, first, "said once");
    view.update(vcx, |v, cx| {
        v.set_state(state_with(CommunityAccess::default()), cx)
    });
    vcx.run_until_parked();
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    assert_ne!(read(vcx).0, first, "a lost read says it too");
    assert!(said.borrow().is_empty(), "no toast");
}

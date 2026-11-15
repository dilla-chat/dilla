//! Task 8's parity, behaviour and keyboard tests for the shell area (standard procedure S4-S6).
//!
//! The parity tests compare each shell fixture with the web client's frame of it. The behaviour tests drive the automation
//! build on the same fixtures (a signed-in device's state, no network): the layout, the splitters and the channel list are
//! the app's own and need no server. What needs a client's preferences (the width a splitter keeps across a reload, the
//! folds and mutes) is tested in the shell's own unit tests (`screens/shell.rs`), and the window shapes in
//! `screens/shell/window.rs`. Run with `cargo test -p dilla-desktop --features automation --test shell`.

#![cfg(feature = "automation")]

mod support;

use std::path::{Path, PathBuf};

use support::world;

use dilla_parity::screen::assert_screen;
use dilla_parity::{Driver, Os};
use gpui::{TestAppContext, VisualTestContext};

fn fixture(name: &str) -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/fixtures")
        .join(format!("{name}.json"))
}

fn launch(name: &str) -> Driver {
    let path = fixture(name);
    let loaded = dilla_parity::fixture::load(&path, Os::current()).expect("the fixture loads");
    Driver::launch_fixture(&path, loaded.shot_path.as_deref(), &[])
}

/// One parity test per fixture: `parity_shell_<state>[_b]`.
macro_rules! parity {
    ($($test:ident => $name:literal $(; $why:literal)?),* $(,)?) => {$(
        #[test]
        $(#[ignore = $why])?
        #[cfg_attr(not(target_os = "linux"), ignore = "references are Linux only")]
        fn $test() {
            assert_screen(&fixture($name))
                .unwrap_or_else(|e| panic!("{} differs from its web reference: {e}", $name));
        }
    )*};
}

parity! {
    parity_shell_conversation => "shell/conversation",
    parity_shell_conversation_b => "shell/conversation-b",
    parity_shell_categories => "shell/categories",
    parity_shell_categories_b => "shell/categories-b",
    parity_shell_panes_resized => "shell/panes-resized",
    parity_shell_panes_resized_b => "shell/panes-resized-b",
    parity_shell_header_tooltip => "shell/header-tooltip",
    parity_shell_header_tooltip_b => "shell/header-tooltip-b",
}

fn rect(app: &mut Driver, id: &str) -> dilla_parity::ElementInfo {
    app.element(id)
        .unwrap_or_else(|| panic!("`{id}` is not on screen"))
}

/// The sidebar's width as the splitter's position gives it: the splitter's hit area is centred on the sidebar's edge.
fn sidebar_width(app: &mut Driver) -> f32 {
    let sidebar = rect(app, "shell-sidebar");
    sidebar.width
}

/// e2e/web/panes.spec.ts `the sidebar splitter drags and takes keys, the width survives a reload, and the aside splitter
/// exists only as a column` (the keys: Enter resets to 244, ArrowRight moves one rem, Home goes to the minimum, End to the
/// maximum; the width across a reload is `sidebar_splitter_drags_takes_keys_and_persists` in `screens/shell.rs`).
#[test]
fn sidebar_splitter_drags_takes_keys_and_persists() {
    let mut app = launch("shell/categories");
    app.wait_idle();
    assert_eq!(sidebar_width(&mut app), 244.0, "the default width");
    app.click("splitter-sidebar");
    assert_eq!(app.focused().as_deref(), Some("splitter-sidebar"));
    app.press("right");
    app.press("right");
    app.wait_idle();
    assert_eq!(
        sidebar_width(&mut app),
        276.0,
        "two ArrowRights are two rem"
    );
    app.press("home");
    app.wait_idle();
    assert_eq!(sidebar_width(&mut app), 192.0, "Home: the 12rem minimum");
    app.press("end");
    app.wait_idle();
    assert_eq!(sidebar_width(&mut app), 384.0, "End: the 24rem maximum");
    app.press("enter");
    app.wait_idle();
    assert_eq!(sidebar_width(&mut app), 244.0, "Enter resets");
    app.press("shift-right");
    app.wait_idle();
    assert_eq!(sidebar_width(&mut app), 308.0, "Shift moves four rem");
}

/// e2e/web/panes.spec.ts `aside_splitter_only_with_aside`: with no aside open there is no aside splitter, and the sidebar's
/// is there.
#[test]
fn aside_splitter_only_with_aside() {
    let mut app = launch("shell/categories");
    app.wait_idle();
    // The member list opens by default at this width (Task 14, as the web's): its column has a splitter.
    assert!(
        app.element("splitter-aside").is_some(),
        "the open member list's column has a splitter"
    );
    app.click("header-members");
    app.wait_idle();
    assert!(app.element("splitter-sidebar").is_some());
    assert!(
        app.element("splitter-aside").is_none(),
        "no aside is drawn: no splitter beside it"
    );
}

/// e2e/web/layout.spec.ts, desktop cases `layout_grammar_holds_at_1280_and_1440`: the rail is 56 wide, the sidebar 244, the
/// top bar and the channel header 44 high, the user panel 64, and the main column takes what is left, at both sizes.
#[test]
fn layout_grammar_holds_at_1280_and_1440() {
    for (name, width, height) in [
        ("shell/conversation", 1280.0, 800.0),
        ("shell/conversation-b", 1440.0, 900.0),
    ] {
        let mut app = launch(name);
        app.wait_idle();
        // The member list opens by default (Task 14); the grammar is measured without an aside, as the references are.
        app.click("header-members");
        app.wait_idle();
        let rail = rect(&mut app, "shell-rail");
        let sidebar = rect(&mut app, "shell-sidebar");
        let main = rect(&mut app, "shell-main");
        let header = rect(&mut app, "shell-header");
        let panel = rect(&mut app, "shell-user-panel");
        let bar = rect(&mut app, "shell-topbar");
        // The probes measure inside the 1px hairline under the bar and the header and over the panel.
        assert_eq!(
            (bar.height, header.height, panel.height),
            (43.0, 43.0, 63.0),
            "{name}"
        );
        assert_eq!((rail.width, sidebar.width), (56.0, 244.0), "{name}");
        assert_eq!(
            sidebar.x,
            rail.x + rail.width,
            "{name}: the sidebar follows the rail"
        );
        assert_eq!(
            main.x,
            sidebar.x + sidebar.width,
            "{name}: main follows the sidebar"
        );
        assert_eq!(
            main.x + main.width,
            width,
            "{name}: main reaches the window's edge"
        );
        assert_eq!(
            panel.y + panel.height,
            height,
            "{name}: the panel ends the window"
        );
        assert_eq!(
            rail.y,
            bar.height + 1.0,
            "{name}: the rail starts under the bar and its hairline"
        );
    }
}

/// e2e/web/afk.spec.ts test 1 `afk_channel_listed_last_for_owner_and_member`: the AFK channel is the list's last row for the
/// server's owner (Ada, in the second fixture) and for a member (Mira, in the first).
#[test]
fn afk_channel_listed_last_for_owner_and_member() {
    for name in ["shell/categories", "shell/categories-b"] {
        let mut app = launch(name);
        app.wait_idle();
        let afk = rect(&mut app, "channel-AFK");
        for other in ["channel-general", "channel-loot"] {
            let row = rect(&mut app, other);
            assert!(afk.y > row.y, "{name}: AFK is below {other}");
        }
    }
}

/// Task 28 (Settings -> Keyboard's "jump to present", MessageLog.tsx `present`): a log scrolled away from its newest
/// message shows the "viewing older messages" bar; Ctrl+End (outside a text field) jumps back and the bar goes.
#[test]
fn ctrl_end_jumps_the_log_back_to_the_present() {
    let mut app = launch("shell/conversation");
    app.wait_idle();
    assert!(
        app.element("log-present").is_none(),
        "at the present: no bar"
    );
    app.scroll("shell-main", 2000.0);
    app.wait_idle();
    assert!(
        app.element("log-present").is_some(),
        "away from the present: the bar"
    );
    // Off the composer (a text field keeps the chord): the rail takes the focus.
    app.press("tab");
    app.wait_idle();
    app.press("ctrl-end");
    app.wait_idle();
    assert!(
        app.element("log-present").is_none(),
        "Ctrl+End jumped back to the present"
    );
}

/// e2e/web/keyboard.spec.ts `messaging_flow_on_keyboard_alone_in_fixed_order` (the shell's part): Tab goes rail, then the
/// channel list, one stop each (the rail and the list rove with the arrows), and the list's arrows step its rows.
#[test]
fn keyboard_shell_focus_order() {
    let mut app = launch("shell/categories");
    app.wait_idle();
    let mut seen = Vec::new();
    for _ in 0..16 {
        app.press("tab");
        if let Some(id) = app.focused()
            && seen.last() != Some(&id)
        {
            seen.push(id);
        }
    }
    let at = |id: &str| seen.iter().position(|s| s == id);
    let rail = at("rail-server-Midgard Crew")
        .unwrap_or_else(|| panic!("the rail's stop is the selected server: {seen:?}"));
    let list = at("focus-channel-general")
        .unwrap_or_else(|| panic!("the list's stop is the selected channel: {seen:?}"));
    assert!(rail < list, "the rail comes before the list: {seen:?}");
    // The order goes on past the list through the panel's buttons, the header's and the composer's stops (focus the app
    // does not name with a probe shows as `-`); the sidebar splitter is the last stop of the tree, after the main column.
    assert!(
        seen.len() >= list + 3,
        "the tab order continues after the list: {seen:?}"
    );
    assert!(
        at("splitter-sidebar").is_some_and(|s| s > list),
        "the splitter comes after the list: {seen:?}"
    );
    // The arrows rove inside the list: Down from the stop goes to the next row, Up back.
    while app.focused().as_deref() != Some("focus-channel-general") {
        app.press("tab");
    }
    // The order of equal positions is the server's (by id): the arrows step to a neighbour, Home and End jump.
    app.press("down");
    let next = app.focused().expect("a row holds the focus");
    assert!(
        next.starts_with("focus-") && next != "focus-channel-general",
        "{next}"
    );
    app.press("end");
    assert_eq!(app.focused().as_deref(), Some("focus-channel-AFK"));
    app.press("up");
    assert_ne!(app.focused().as_deref(), Some("focus-channel-AFK"));
    app.press("home");
    let first = app.focused().expect("a row holds the focus");
    assert!(first.starts_with("focus-"), "{first}");
}

/// A category's head folds its channels (the web: `aria-expanded`), and the fold is only a view: the selected channel stays.
#[test]
fn a_category_head_folds_its_channels() {
    let mut app = launch("shell/categories");
    app.wait_idle();
    assert!(app.element("channel-harbour").is_some());
    app.click("category-boats");
    app.wait_idle();
    assert!(
        app.element("channel-harbour").is_none(),
        "folded: harbour is hidden"
    );
    assert!(app.element("channel-general").is_some());
    app.click("category-boats");
    app.wait_idle();
    assert!(app.element("channel-harbour").is_some(), "unfolded again");
}

/// e2e/web/layout.spec.ts desktop `desktop_home_title_is_not_server_name`: the window's title is the product's, whatever
/// server is shown.
#[gpui::test]
fn desktop_home_title_is_not_server_name(cx: &mut TestAppContext) {
    let loaded = dilla_desktop::fixture::Fixture::load(&fixture("shell/categories"), None)
        .expect("the fixture loads");
    let handle = cx
        .update(|cx| {
            dilla_desktop::open_main_window_with(cx, dilla_desktop::Launch::fixture(loaded))
        })
        .expect("the main window opens");
    let mut window_cx = VisualTestContext::from_window(handle.into(), cx);
    assert_eq!(window_cx.window_title().as_deref(), Some("Dilla"));
}

fn bounds(width: f32, height: f32) -> gpui::Bounds<gpui::Pixels> {
    gpui::Bounds::new(
        gpui::point(gpui::px(0.0), gpui::px(0.0)),
        gpui::size(gpui::px(width), gpui::px(height)),
    )
}

/// The former Electron shell (tag desktop-v0.1.0-beta.5, packages/desktop/test/electron/shell.spec.ts) `startup_and_sign_in_compact_window_expands_for_workspace`: the app starts
/// in the 400x280 start window (not resizable), the sign-in takes the compact authentication shape (still 400 wide and
/// fixed), and the workspace is 1240 wide and resizable with a 900x600 minimum.
#[gpui::test]
fn startup_and_sign_in_compact_window_expands_for_workspace(cx: &mut TestAppContext) {
    use dilla_desktop::screens::shell::window::{WindowMode, window_shape};
    let start = window_shape(WindowMode::Starting, None, 1080.0);
    assert_eq!(
        (start.width, start.height, start.resizable),
        (400.0, 280.0, false)
    );
    let auth = window_shape(WindowMode::Authentication, None, 1080.0);
    assert_eq!((auth.width, auth.resizable), (400.0, false));
    assert!(auth.height >= 220.0);
    let work = window_shape(WindowMode::Workspace, None, 1080.0);
    assert_eq!(
        (work.width, work.min_width, work.min_height, work.resizable),
        (1240.0, 900.0, 600.0, true)
    );

    // The options the shipping window opens with, per mode.
    let start_options =
        dilla_desktop::window_options_for(WindowMode::Starting, bounds(400.0, 280.0));
    assert!(!start_options.is_resizable);
    let work_options =
        dilla_desktop::window_options_for(WindowMode::Workspace, bounds(1240.0, 820.0));
    assert!(work_options.is_resizable);

    // A shipping launch with no instance opens at the start size, then fits the sign-in's compact shape: 400 wide, fixed.
    let mut launch = dilla_desktop::Launch::empty();
    launch.shipping = true;
    let handle = cx
        .update(|cx| dilla_desktop::open_main_window_with(cx, launch))
        .expect("the start window opens");
    let (width, resizable) = handle
        .update(cx, |_, window, _| {
            (f32::from(window.bounds().size.width), window.is_resizable())
        })
        .expect("the window is open");
    assert_eq!(width, 400.0);
    assert!(!resizable, "the start and sign-in windows are fixed");

    // The workspace window.
    let loaded = dilla_desktop::fixture::Fixture::load(&fixture("shell/categories"), None).unwrap();
    let handle = cx
        .update(|cx| {
            dilla_desktop::open_window_in(
                cx,
                dilla_desktop::Launch::fixture(loaded),
                WindowMode::Workspace,
            )
        })
        .expect("the workspace window opens");
    let (width, resizable) = handle
        .update(cx, |_, window, _| {
            (f32::from(window.bounds().size.width), window.is_resizable())
        })
        .expect("the window is open");
    assert!(width >= 900.0);
    assert!(resizable);
}

/// The former Electron shell (tag desktop-v0.1.0-beta.5, packages/desktop/test/electron/shell.spec.ts) `resize_cannot_shrink_below_desktop_minimum`: the workspace window carries
/// a 900x600 minimum (the platform enforces it on every resize), the compact windows are not resizable, and bounds kept
/// under the minimum are restored at it.
#[test]
fn resize_cannot_shrink_below_desktop_minimum() {
    use dilla_desktop::screens::shell::window::{
        SavedBounds, WindowMode, restore_bounds, window_shape,
    };
    let options = dilla_desktop::window_options_for(WindowMode::Workspace, bounds(1240.0, 820.0));
    let min = options
        .window_min_size
        .expect("the workspace has a minimum size");
    assert_eq!(
        (f32::from(min.width), f32::from(min.height)),
        (900.0, 600.0)
    );
    let main = dilla_desktop::main_window_options(bounds(1240.0, 820.0));
    assert_eq!(main.window_min_size, options.window_min_size);
    let compact =
        dilla_desktop::window_options_for(WindowMode::Authentication, bounds(400.0, 620.0));
    assert!(!compact.is_resizable);
    let shape = window_shape(WindowMode::Workspace, None, 1080.0);
    let saved = SavedBounds {
        x: 0.0,
        y: 0.0,
        width: 500.0,
        height: 300.0,
    };
    let (w, h, _) = restore_bounds(Some(saved), shape, &[]);
    assert_eq!((w, h), (900.0, 600.0));
}

// ---- Package 10d: the revoked device's and the lost store's boot ----

/// Runs the window until `done` holds (the client's start runs on its own thread, the wipe in the background).
fn until_screen(
    vcx: &mut VisualTestContext,
    what: &str,
    mut done: impl FnMut(&mut VisualTestContext) -> bool,
) {
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(60);
    while !done(vcx) {
        assert!(std::time::Instant::now() < deadline, "timed out: {what}");
        vcx.run_until_parked();
        vcx.executor()
            .advance_clock(std::time::Duration::from_millis(20));
        std::thread::sleep(std::time::Duration::from_millis(20));
    }
    vcx.run_until_parked();
}

/// The window on `root`'s first instance, started as the shipping app starts it (resume), with `kc`.
fn boot_window<'a>(
    cx: &'a mut TestAppContext,
    root: &Path,
    kc: std::sync::Arc<dilla_client::keychain::MemoryKeychain>,
) -> (
    gpui::Entity<dilla_desktop::app::AppView>,
    &'a mut VisualTestContext,
) {
    cx.executor().allow_parking();
    cx.update(dilla_desktop::install);
    let mut launch = dilla_desktop::app::Launch::live(Some(root.to_path_buf()), kc);
    launch.shipping = false;
    let (app, vcx) =
        cx.add_window_view(move |window, cx| dilla_desktop::app::AppView::new(launch, window, cx));
    (app, vcx)
}

/// After the confirmed reset: the instance is forgotten here (its list entry, data dir and key gone) and the window
/// is on the sign-in, where it is added again.
fn reset_and_expect_forgotten(
    app: &gpui::Entity<dilla_desktop::app::AppView>,
    vcx: &mut VisualTestContext,
    root: &Path,
    cfg: &dilla_client::seams::InstanceConfig,
) {
    app.update_in(vcx, |a, window, cx| a.confirm_boot_reset(window, cx));
    until_screen(vcx, "the sign-in after the reset", |vcx| {
        app.read_with(vcx, |a, _| a.shows_sign_in())
    });
    let listed = dilla_client::instances::Instances::load(root).unwrap();
    assert!(listed.get(cfg.id).is_none(), "the instance is forgotten");
    assert!(!cfg.data_dir.exists(), "its data is wiped");
}

/// e2e/web/z/reload.spec.ts "a profile whose device key wrapper is gone says so and can start over after a
/// confirmation": the store's key is gone from the keychain (a reset keychain), so the start says the data can no longer
/// be opened (`boot.storeLost.*`) instead of failing, and Reset this browser, after its confirmation, wipes the data the
/// key opened and leaves the window on the sign-in. Nothing is wiped before the confirmation.
#[gpui::test]
fn lost_key_boot_offers_reset(cx: &mut TestAppContext) {
    use dilla_client::seams::Keychain as _;
    let root = Path::new(env!("CARGO_TARGET_TMPDIR"))
        .join(format!("p10d-store-lost-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&root);
    let cfg = dilla_client::instances::Instances::load(&root)
        .unwrap()
        .add(url::Url::parse("https://chat.example.org").unwrap())
        .unwrap();
    let kc = std::sync::Arc::new(dilla_client::keychain::MemoryKeychain::new());
    drop(dilla_client::store::open_store(&cfg, kc.as_ref()).unwrap());
    let account = dilla_client::store::store_keychain_account(&cfg).unwrap();
    kc.delete(&account).unwrap();

    let (app, vcx) = boot_window(cx, &root, kc);
    until_screen(vcx, "the failure splash", |vcx| {
        app.read_with(vcx, |a, _| a.screen_name() == "fatal")
    });
    assert_eq!(
        app.read_with(vcx, |a, _| a.failure_boot()),
        Some(dilla_desktop::screens::boot::Boot::StoreLost)
    );
    assert!(
        cfg.data_dir.exists(),
        "nothing is wiped before the confirmation"
    );
    reset_and_expect_forgotten(&app, vcx, &root, &cfg);
    let _ = std::fs::remove_dir_all(&root);
}

/// e2e/web/required-factor.spec.ts "the forced screen's Forget this browser clears this browser and leaves the account's
/// device list" (the boot half, Boot.tsx `revoked`): Ada removes her first device from a second one; the first device's
/// next start is refused, so it explains that the account no longer accepts it (`boot.revoked.*`) instead of failing,
/// and Forget this browser, after its confirmation, wipes it here and leaves the window on the sign-in.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn revoked_device_boot_explains_and_resets(cx: &mut TestAppContext) {
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let tag = "p10d-revoked";
    let host = world::start_host(tag, "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let mut ada = rt.block_on(world::sign_up(&host, &invite, "ada", "Ada", tag));
    world::community(&host, &ada, "Revoked", &[]);
    world::clear_session(&ada);
    let first = dilla_client::state::DeviceId(ada.device_id);
    let (second, second_root) = rt.block_on(world::second_device(&host, &mut ada, tag));
    rt.block_on(async {
        second
            .devices()
            .revoke(
                first,
                Some(zeroize::Zeroizing::new(ada.recovery_key.clone())),
            )
            .await
            .expect("the client answers")
            .expect("the second device removes the first");
    });
    second.stop();
    let cfg = ada.cfg.clone();
    let root = cfg
        .data_dir
        .parent()
        .and_then(|d| d.parent())
        .unwrap()
        .to_path_buf();

    let (app, vcx) = boot_window(cx, &root, ada.kc.clone());
    until_screen(vcx, "the failure splash", |vcx| {
        app.read_with(vcx, |a, _| a.screen_name() == "fatal")
    });
    assert_eq!(
        app.read_with(vcx, |a, _| a.failure_boot()),
        Some(dilla_desktop::screens::boot::Boot::Revoked)
    );
    reset_and_expect_forgotten(&app, vcx, &root, &cfg);
    drop(rt);
    let _ = std::fs::remove_dir_all(second_root);
}

/// Package 10d: the revoked device's and the lost store's boot splashes (Boot.tsx), against their web references.
#[test]
#[cfg_attr(not(target_os = "linux"), ignore = "references are Linux only")]
fn parity_boot_revoked() {
    assert_screen(&fixture("boot/revoked")).unwrap_or_else(|f| panic!("{f}"));
}

#[test]
#[cfg_attr(not(target_os = "linux"), ignore = "references are Linux only")]
fn parity_boot_store_lost() {
    assert_screen(&fixture("boot/store-lost")).unwrap_or_else(|f| panic!("{f}"));
}
